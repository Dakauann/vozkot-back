// Package payment settles orders against a payment provider.
//
// Two rules run through everything here:
//
//  1. A webhook is a doorbell, not a delivery. It says a payment changed; what
//     it changed into is always read back from the provider, because anyone can
//     POST a body that says "approved".
//  2. Every path is safe to run twice. Providers redeliver, queues retry, and
//     an operator may replay a sync by hand — so state transitions report
//     whether they changed anything, and stock only moves when they did.
package payment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	"vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/domain/uow"
	notificationUsecase "vozkot/usecases/notification"
	queueUsecase "vozkot/usecases/queue"
)

type Service struct {
	unit    uow.Runner
	orders  orderdomain.Repository
	gateway paymentdomain.Gateway
	jobs    queue.Queue
	// notifications is what the buyer hears about all this. Nil is supported
	// and means no provider is configured: settlement is unchanged and no
	// message is queued that could never be sent.
	notifications *notificationUsecase.Purchases
	dispatcher    *queueUsecase.Dispatcher
	now           func() time.Time
	newID         func() string
}

func NewService(
	unit uow.Runner,
	orders orderdomain.Repository,
	gateway paymentdomain.Gateway,
	jobs queue.Queue,
	dispatcher *queueUsecase.Dispatcher,
	notifications *notificationUsecase.Purchases,
) *Service {
	return &Service{
		unit:          unit,
		orders:        orders,
		gateway:       gateway,
		jobs:          jobs,
		notifications: notifications,
		dispatcher:    dispatcher,
		now:           time.Now,
		newID:         randomID,
	}
}

// CreateCharge asks the provider for the charge an order is waiting on.
//
// Idempotent three times over: an order that is no longer pending is skipped, an
// order that already has a payment id is reconciled instead of charged again,
// and the request itself carries the order id as the provider's idempotency
// key, so even a duplicate call that gets past both checks returns the original
// charge rather than creating a second one.
func (s *Service) CreateCharge(ctx context.Context, orderID string) error {
	item, err := s.orders.GetByID(ctx, orderID)
	if errors.Is(err, orderdomain.ErrNotFound) {
		// No retry will conjure the order. Parked, and loud in the logs.
		return queue.Permanent(fmt.Errorf("charge order %s: %w", orderID, err))
	}
	if err != nil {
		return err
	}
	if item.Status != orderdomain.StatusPendingPayment {
		return nil
	}
	if item.PaymentID != "" {
		return s.SyncOrder(ctx, item.ID)
	}
	if s.gateway == nil {
		return paymentdomain.ErrNotConfigured
	}

	charge, err := s.gateway.CreateCharge(ctx, paymentdomain.ChargeRequest{
		Method:            item.PaymentMethod,
		AmountCents:       item.TotalCents,
		Currency:          item.Currency,
		Description:       fmt.Sprintf("Ingresso %s x%d", item.TicketID, item.Quantity),
		ExternalReference: item.ID,
		// The charge dies with the hold. A PIX code that outlives the
		// reservation invites a payment for tickets that are already back on
		// sale, which is money the box office then has to give back.
		ExpiresAt: item.HoldExpiresAt,
		Customer: paymentdomain.Customer{
			Name:     item.BuyerName,
			Email:    item.BuyerEmail,
			Document: item.BuyerDocument,
		},
		IdempotencyKey: item.ID,
	})
	if err != nil {
		return err
	}

	// The charge is applied through settle, never written straight onto the
	// copy read before the provider was called.
	//
	// That read is now old. Creating a charge takes a round trip to Mercado
	// Pago — a few hundred milliseconds normally, and up to the whole retry
	// budget when the provider is failing — and the order can move during it:
	// the hold expires and the tickets go back on sale, a webhook settles it,
	// an admin cancels it. Writing this stale copy back would silently undo
	// that, resurrecting an expired order to pending_payment while its stock
	// belongs to somebody else. settle re-reads the row under FOR UPDATE and
	// is the only place that writes it.
	//
	// It also covers the charge that is born final — an already-approved
	// payment, or one the provider rejects outright — which must be settled
	// now rather than waiting for a webhook that may never come.
	return s.settle(ctx, item.ID, charge)
}

// SyncOrder reconciles one order against the provider.
func (s *Service) SyncOrder(ctx context.Context, orderID string) error {
	item, err := s.orders.GetByID(ctx, orderID)
	if errors.Is(err, orderdomain.ErrNotFound) {
		return queue.Permanent(fmt.Errorf("sync order %s: %w", orderID, err))
	}
	if err != nil {
		return err
	}
	if item.PaymentID == "" {
		// Nothing was ever charged; the charge job is the right step, not this.
		return s.CreateCharge(ctx, item.ID)
	}
	charge, err := s.fetch(ctx, item.PaymentID)
	if err != nil {
		return err
	}
	return s.settle(ctx, item.ID, charge)
}

// SyncPayment reconciles by provider payment id, which is all a webhook knows.
//
// The order is found by payment id, and failing that by the charge's external
// reference — which covers the genuine race where the provider's notification
// arrives before the charge response was persisted.
func (s *Service) SyncPayment(ctx context.Context, paymentID string) error {
	charge, err := s.fetch(ctx, paymentID)
	if err != nil {
		return err
	}

	item, err := s.orders.FindByPaymentID(ctx, string(charge.Provider), charge.ID)
	if errors.Is(err, orderdomain.ErrNotFound) && charge.ExternalReference != "" {
		item, err = s.orders.GetByID(ctx, charge.ExternalReference)
	}
	if errors.Is(err, orderdomain.ErrNotFound) {
		// A payment that belongs to nothing here. Another application may share
		// the provider account; acknowledging and ignoring is correct, and
		// retrying would never find it.
		log.Printf("payment: notification for unknown charge %s", charge.ID)
		return nil
	}
	if err != nil {
		return err
	}
	return s.settle(ctx, item.ID, charge)
}

// ErrNotRefundable is an order that never took money, or already gave it back.
var ErrNotRefundable = errors.New("order has no settled charge to refund")

// RequestRefund schedules a refund and returns the order as it stands.
//
// The provider call is NOT made here, and that is the point. A refund is a
// round trip to a third party that is occasionally slow and occasionally down,
// and running it on the request meant a provider slower than the HTTP write
// timeout left the money refunded at Mercado Pago and the order untouched here
// — the two facts that must never disagree, disagreeing, with nothing left to
// reconcile them. The job row is durable, retried with backoff, and keyed on
// the order, so an operator double-clicking refunds once.
//
// What IS checked here is everything a person should learn immediately: an
// order that does not exist, or one there is nothing to refund on. Those would
// only ever park a job and wait for someone to read the log.
func (s *Service) RequestRefund(ctx context.Context, orderID string) (*orderdomain.Order, error) {
	item, err := s.orders.GetByID(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if s.gateway == nil {
		return nil, paymentdomain.ErrNotConfigured
	}
	if item.PaymentID == "" {
		return nil, fmt.Errorf("%w: order %s was never charged", ErrNotRefundable, orderID)
	}
	if item.Status != orderdomain.StatusPaid && item.Status != orderdomain.StatusRefundRequired {
		return nil, fmt.Errorf("%w: order %s is %s", ErrNotRefundable, orderID, item.Status)
	}

	payload, err := queue.NewPayload(queue.SyncPaymentPayload{OrderID: item.ID, PaymentID: item.PaymentID})
	if err != nil {
		return nil, err
	}
	job := &queue.Job{
		ID:          s.newID(),
		Type:        queue.TypeRefundCharge,
		Payload:     payload,
		RunAt:       s.now(),
		MaxAttempts: queue.DefaultMaxAttempts,
		// One refund per order, however many times the button is pressed.
		DedupeKey: queue.TypeRefundCharge + ":" + item.ID,
	}
	added, err := s.jobs.Enqueue(ctx, job)
	if err != nil {
		return nil, err
	}
	if added {
		s.dispatcher.Dispatch(ctx, job)
	}
	return item, nil
}

// Refund gives the money back and returns the tickets to sale. It is the
// handler behind TypeRefundCharge.
//
// Safe to run twice at every level: an order that has already been refunded is
// skipped, the provider is given a key derived from the charge, and the outcome
// is read back rather than assumed.
func (s *Service) Refund(ctx context.Context, orderID string) error {
	item, err := s.orders.GetByID(ctx, orderID)
	if errors.Is(err, orderdomain.ErrNotFound) {
		return queue.Permanent(fmt.Errorf("refund order %s: %w", orderID, err))
	}
	if err != nil {
		return err
	}
	if item.Status == orderdomain.StatusRefunded {
		// Already given back — a redelivered job, or an operator who pressed
		// the button while the first refund was settling.
		return nil
	}
	if item.PaymentID == "" {
		return queue.Permanent(fmt.Errorf("%w: order %s was never charged", ErrNotRefundable, orderID))
	}
	if s.gateway == nil {
		return paymentdomain.ErrNotConfigured
	}
	if err := s.gateway.RefundCharge(ctx, item.PaymentID, item.TotalCents); err != nil {
		if !paymentdomain.Retryable(err) {
			// A rejected refund is not going to start working on attempt eight;
			// park it where a person will see it.
			return queue.Permanent(fmt.Errorf("refund order %s: %w", orderID, err))
		}
		return err
	}
	// The refund is confirmed by reading the charge back, not by assuming the
	// call worked.
	return s.SyncOrder(ctx, item.ID)
}

// AuditSettled re-reads orders that are already PAID against the provider.
//
// Reconcile only ever looks at orders still waiting for money, which leaves one
// hole: a refund or a chargeback raised in the provider's own dashboard reaches
// the box office through exactly one notification. Lose that delivery and the
// order stays paid and the seat stays sold for good — the money went back and
// the inventory never did, and nothing in the system would ever notice.
//
// It runs hourly rather than every minute, and only for events that have not
// happened yet. Re-reading every order ever paid, forever, would be unbounded
// work for no benefit: once the doors have closed a late refund is bookkeeping,
// not a ticket someone else could have bought.
func (s *Service) AuditSettled(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	pageSize := limit
	if pageSize > 500 {
		pageSize = 500
	}
	maxScanned := limit * 10
	if maxScanned < 1000 {
		maxScanned = 1000
	}

	scheduled := 0
	for offset := 0; scheduled < limit && offset < maxScanned; offset += pageSize {
		paid, err := s.orders.List(ctx, orderdomain.Filter{
			Status:             orderdomain.StatusPaid,
			Limit:              pageSize,
			Offset:             offset,
			OldestUpdatedFirst: true,
			EventNotBefore:     s.now(),
		})
		if err != nil {
			return scheduled, err
		}
		for index := range paid {
			added, err := s.scheduleReconciliation(ctx, &paid[index])
			if err != nil {
				return scheduled, err
			}
			if added {
				scheduled++
				if scheduled == limit {
					break
				}
			}
		}
		if len(paid) < pageSize {
			break
		}
	}
	return scheduled, nil
}

// Reconcile re-checks orders that are still waiting, and is what makes the
// system correct even if every webhook is lost.
//
// A payment integration that only works when notifications are delivered is a
// payment integration that eventually takes someone's money without giving them
// a ticket.
func (s *Service) Reconcile(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	// Read in bounded pages, but count only jobs actually added against the
	// limit. The earlier implementation read the newest `limit` rows once. If
	// those rows already had open jobs, every sweep revisited the same rows and
	// older payments could be starved forever.
	pageSize := limit
	if pageSize > 500 {
		pageSize = 500
	}
	maxScanned := limit * 10
	if maxScanned < 1000 {
		maxScanned = 1000
	}
	scheduled := 0
	for offset := 0; scheduled < limit && offset < maxScanned; offset += pageSize {
		pending, err := s.orders.List(ctx, orderdomain.Filter{
			Status:             orderdomain.StatusPendingPayment,
			Limit:              pageSize,
			Offset:             offset,
			OldestUpdatedFirst: true,
		})
		if err != nil {
			return scheduled, err
		}
		for index := range pending {
			added, err := s.scheduleReconciliation(ctx, &pending[index])
			if err != nil {
				return scheduled, err
			}
			if added {
				scheduled++
				if scheduled == limit {
					break
				}
			}
		}
		if len(pending) < pageSize {
			break
		}
	}
	return scheduled, nil
}

// ScheduleReconciliation adds the correct recovery job for one order. It is
// exported for scoped operational tools such as the load harness; the regular
// production sweep calls Reconcile.
func (s *Service) ScheduleReconciliation(ctx context.Context, orderID string) (bool, error) {
	item, err := s.orders.GetByID(ctx, orderID)
	if errors.Is(err, orderdomain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if item.Status != orderdomain.StatusPendingPayment {
		return false, nil
	}
	return s.scheduleReconciliation(ctx, item)
}

func (s *Service) scheduleReconciliation(ctx context.Context, item *orderdomain.Order) (bool, error) {
	jobType := queue.TypeSyncPayment
	dedupeID := item.PaymentID
	if item.PaymentID == "" {
		jobType = queue.TypeCreateCharge
		dedupeID = item.ID
	}
	payload, err := queue.NewPayload(queue.SyncPaymentPayload{OrderID: item.ID, PaymentID: item.PaymentID})
	if err != nil {
		return false, err
	}
	job := &queue.Job{
		ID:          s.newID(),
		Type:        jobType,
		Payload:     payload,
		RunAt:       s.now(),
		MaxAttempts: queue.DefaultMaxAttempts,
		// Match the keys used by checkout and webhooks. A recovery sweep racing
		// either path is the same work, not a second provider call.
		DedupeKey: jobType + ":" + dedupeID,
	}
	added, err := s.jobs.Enqueue(ctx, job)
	if err != nil {
		return false, err
	}
	if added {
		s.dispatcher.Dispatch(ctx, job)
	}
	return added, nil
}

// settle applies a charge's state to its order, moving stock to match.
//
// This is the only place in the system where money and inventory meet, and it
// runs inside one transaction: an order marked paid whose stock was not
// committed oversells the next buyer, and stock committed for an order that
// stayed pending sells the same seat twice.
func (s *Service) settle(ctx context.Context, orderID string, charge *paymentdomain.Charge) error {
	if charge == nil {
		return nil
	}

	// Messages raised inside the transaction and announced to the broker only
	// after it commits, which is the same ordering checkout uses and for the
	// same reason: a receipt published for a settlement that then rolled back
	// tells a buyer they own tickets they do not.
	var messages []*queue.Job

	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		// Locked for the rest of the transaction. Two deliveries of the same
		// approval processed at once now take turns, and the second reads the
		// paid order the first committed — the redelivery case handled by the
		// "did this change anything" check below, not by an error.
		item, err := repositories.Orders().GetByIDForUpdate(ctx, orderID)
		if err != nil {
			return err
		}

		previous := item.Status
		// What the ROW knew, captured before the charge overwrites it.
		storedPaymentID := item.PaymentID
		item.AttachCharge(charge, s.now())

		next, moves := orderdomain.StatusFor(charge.Status)
		if !moves {
			// Pending or in analysis: record what the provider says and leave
			// the order where it is.
			//
			// Unless the order is already finished and already carries a
			// charge, in which case this news is older than what the order
			// knows — the provider's answer to "create the charge" overtaken
			// by the webhook that settled it — and older news is not recorded
			// over newer.
			if previous.Final() && storedPaymentID != "" {
				return nil
			}
			if err := repositories.Orders().Update(ctx, item); err != nil {
				return err
			}
			// The provider has answered with something the buyer can act on,
			// and this is the first time the order has carried it. Sending
			// this at checkout instead would have promised a PIX code that did
			// not exist yet.
			if previous.HoldsStock() && storedPaymentID == "" && item.PixCopyPaste != "" {
				message, err := s.raise(ctx, repositories, item, s.notifications.ChargeIssued)
				if err != nil {
					return err
				}
				messages = append(messages, message)
			}
			return nil
		}

		// Whether an approval can actually be honoured is decided BEFORE the
		// order is moved, not after. Applying "paid" first and discovering the
		// tickets are gone second would leave the order in a state the machine
		// cannot legally move out of — and the honest answer, "we owe a
		// refund", would be unreachable.
		reservedLate := false
		if next == orderdomain.StatusPaid && !previous.HoldsStock() {
			// The hold lapsed and the tickets went back on sale. Paying for an
			// expired reservation is frequent — a buyer opens the bank app,
			// gets distracted, pays eleven minutes later — so the stock is
			// asked for again rather than assumed.
			reserved, err := repositories.Tickets().Reserve(ctx, item.TicketID, item.Quantity)
			if err != nil {
				return err
			}
			if reserved {
				reservedLate = true
			} else {
				// Sold out in the meantime. The money is real and the tickets
				// are gone, so the order says exactly that and someone owes a
				// refund. Marking it paid would oversell the event.
				log.Printf("payment: order %s paid after its hold expired and the tickets are gone; refund required", item.ID)
				next = orderdomain.StatusRefundRequired
			}
		}

		changed, err := item.Apply(next, s.now())
		if err != nil {
			if reservedLate {
				if releaseErr := repositories.Tickets().Release(ctx, item.TicketID, item.Quantity); releaseErr != nil {
					return releaseErr
				}
			}
			if errors.Is(err, orderdomain.ErrInvalidTransition) {
				// A late or out-of-order event, such as an approval arriving
				// for an order that was already refunded. The payment fields
				// are still worth recording; the status is not moved.
				log.Printf("payment: refusing %s -> %s for order %s", previous, next, item.ID)
				return repositories.Orders().Update(ctx, item)
			}
			return err
		}
		if !changed {
			// The same event again. This is the redelivery case and it must not
			// touch stock a second time.
			if reservedLate {
				if releaseErr := repositories.Tickets().Release(ctx, item.TicketID, item.Quantity); releaseErr != nil {
					return releaseErr
				}
			}
			return repositories.Orders().Update(ctx, item)
		}

		switch next {
		case orderdomain.StatusPaid:
			// Either the hold this order already had, or the one just taken
			// back for it.
			if err := repositories.Tickets().Commit(ctx, item.TicketID, item.Quantity); err != nil {
				return err
			}
		case orderdomain.StatusExpired, orderdomain.StatusFailed, orderdomain.StatusCancelled:
			if previous.HoldsStock() {
				if err := repositories.Tickets().Release(ctx, item.TicketID, item.Quantity); err != nil {
					return err
				}
			}
		case orderdomain.StatusRefunded:
			// Only stock that was actually sold comes back; refunding an order
			// that never completed would credit inventory already released.
			if previous == orderdomain.StatusPaid {
				if err := repositories.Tickets().ReleaseSold(ctx, item.TicketID, item.Quantity); err != nil {
					return err
				}
			}
		}

		if err := repositories.Orders().Update(ctx, item); err != nil {
			return err
		}
		// Raised here and nowhere else. This is the one line in the system
		// that knows a sale just became real, and it is inside the transaction
		// that made it real: the receipt commits with the money or neither
		// does. `changed` guarantees exactly one crossing, so a webhook
		// delivered five times still buys one email.
		if next == orderdomain.StatusPaid {
			message, err := s.raise(ctx, repositories, item, s.notifications.OrderPaid)
			if err != nil {
				return err
			}
			messages = append(messages, message)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.dispatcher.Dispatch(ctx, messages...)
	return nil
}

// raise queues one buyer message from inside the settlement transaction.
//
// The tier is read for the event details the message carries — the show, the
// door time, the venue — because a receipt that says only "R$ 240,00" is not a
// receipt. A tier that has been deleted costs those details and not the
// settlement: the message still goes out, naming the order.
func (s *Service) raise(
	ctx context.Context,
	repositories uow.Repositories,
	item *orderdomain.Order,
	compose func(context.Context, queue.Queue, *orderdomain.Order, *ticketdomain.Ticket) (*queue.Job, error),
) (*queue.Job, error) {
	if s.notifications == nil {
		// No provider configured: skip the read as well as the message.
		return nil, nil
	}
	tier, err := repositories.Tickets().GetByID(ctx, item.TicketID)
	if err != nil && !errors.Is(err, ticketdomain.ErrNotFound) {
		return nil, err
	}
	return compose(ctx, repositories.Jobs(), item, tier)
}

func (s *Service) fetch(ctx context.Context, chargeID string) (*paymentdomain.Charge, error) {
	if s.gateway == nil {
		return nil, paymentdomain.ErrNotConfigured
	}
	charge, err := s.gateway.GetCharge(ctx, chargeID)
	if err != nil {
		return nil, err
	}
	if charge == nil {
		return nil, paymentdomain.ErrChargeNotFound
	}
	return charge, nil
}

// ErrTicketGone is returned when a settled order's ticket no longer exists,
// which can only happen if a tier was deleted while an order was open.
var ErrTicketGone = ticketdomain.ErrNotFound

func randomID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "job_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return "job_" + hex.EncodeToString(buffer)
}
