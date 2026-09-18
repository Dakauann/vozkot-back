// Package payment settles orders against a payment provider.
//
// Two rules run through everything here:
//
//  1. A webhook is a doorbell, not a delivery. It says a payment changed; what
//     it changed into is always read back from the provider, because anyone can
//     POST a body that says "approved".
//  2. Every path is safe to run twice. Providers redeliver, queues retry, and
//     an operator may replay a sync by hand, so state transitions report
//     whether they changed anything, and stock only moves when they did.
package payment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	admissiondomain "vozkot/domain/admission"
	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	ledgerdomain "vozkot/domain/ledger"
	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	"vozkot/domain/queue"
	seatingdomain "vozkot/domain/seating"
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
	// ledger is the organiser's balance. Nil is supported and means a
	// deployment that has not turned payouts on: settlement is unchanged and
	// no entry is written that nothing would ever pay out.
	ledger accruer
	// refunds opens the box office's own refund when settlement finds the
	// tickets gone. Nil is supported and leaves the order in refund_required
	// for a person, which is what this system did before.
	refunds refunder
	// reconcileAfter is how long an order must have gone untouched before a
	// recovery sweep re-reads it from the provider.
	reconcileAfter time.Duration
	dispatcher     *queueUsecase.Dispatcher
	now            func() time.Time
	newID          func() string
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
		unit:           unit,
		orders:         orders,
		gateway:        gateway,
		jobs:           jobs,
		notifications:  notifications,
		dispatcher:     dispatcher,
		now:            time.Now,
		newID:          randomID,
		reconcileAfter: DefaultReconcileAfter,
	}
}

// DefaultReconcileAfter is how stale a pending order must be before the
// recovery sweep pays for a provider call about it.
//
// THE NUMBER IS A TRADE, and both directions cost something real. Too long and
// a buyer whose webhook was lost waits that much longer to be told they own
// their tickets. Too short and the sweep becomes a poller: it re-reads every
// unpaid order every pass, spending a provider call each time to discover that
// somebody has not opened their bank app yet. At a minute, one abandoned cart
// costs thirty calls over a thirty-minute hold; at three, ten.
//
// Three minutes is chosen against the real shape of PIX: most payments land in
// under two, so with webhooks working the sweep usually finds nothing to do at
// all, which is exactly what a safety net should cost.
const DefaultReconcileAfter = 3 * time.Minute

// WithReconcileAfter overrides the staleness threshold.
func (s *Service) WithReconcileAfter(after time.Duration) *Service {
	if after > 0 {
		s.reconcileAfter = after
	}
	return s
}

// accruer is the slice of usecases/payout this file uses.
//
// An interface declared HERE, at the consumer, rather than a package import of
// the concrete service: it keeps the dependency one-directional and it is the
// two methods this file actually calls, so a reader of settle() can see the
// whole contract without opening another package.
type accruer interface {
	Accrue(ctx context.Context, repositories uow.Repositories, item *orderdomain.Order) error
	Reverse(ctx context.Context, repositories uow.Repositories, item *orderdomain.Order, kind ledgerdomain.Kind) error
}

// WithLedger turns organiser accrual on.
//
// Optional in the same way notifications are, and for the same reason: a
// deployment with no payouts configured must settle payments exactly as it did
// before rather than fail on a dependency it does not have.
func (s *Service) WithLedger(ledger accruer) *Service {
	s.ledger = ledger
	return s
}

// refunder is the slice of usecases/refund this file uses.
//
// Declared HERE, at the consumer, for the same reason accruer is, plus one this
// package cannot avoid: usecases/refund already imports this package, because
// the payment service is its Money port. Importing it back would be a cycle, so
// the dependency is inverted at the seam and the container joins the two ends.
type refunder interface {
	OpenOperatorRefund(ctx context.Context, repositories uow.Repositories, item *orderdomain.Order) (*queue.Job, error)
}

// WithRefunds turns the automatic operator refund on.
//
// Optional like the ledger. Without it settlement still records
// refund_required, which is the honest state, and the money waits for a person.
func (s *Service) WithRefunds(refunds refunder) *Service {
	s.refunds = refunds
	return s
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

	// The last gate before money moves.
	//
	// This order was priced by domain/order at checkout and has been sitting in
	// Postgres since; what gets charged below is item.TotalCents, and what the
	// organiser is eventually owed is item.SubtotalCents. If those two have
	// stopped agreeing with each other the buyer and the payout are about to be
	// computed from different numbers, and no later reconciliation can undo a
	// PIX that has already been paid. Permanent rather than retried: the row
	// will not fix itself, and a retry loop would keep re-attempting a bad
	// charge instead of surfacing it.
	if err := item.ValidatePricing(); err != nil {
		return queue.Permanent(fmt.Errorf("charge order %s: %w", item.ID, err))
	}

	// One line per charge, carrying the whole split.
	//
	// This is the record that lets an operator answer "why was I charged this"
	// from the logs alone, without a database session: the face value the
	// organiser set, the commission this build adds, and the sum the provider
	// was asked for. It is logged BEFORE the call, so a charge that is created
	// and then loses its response still leaves the amount behind.
	log.Printf("pricing: order %s charging %s %d = tickets %d + service fee %d (%d items)",
		item.ID, item.Currency, item.TotalCents, item.SubtotalCents, item.BuyerFeeCents, item.TotalQuantity())

	charge, err := s.gateway.CreateCharge(ctx, paymentdomain.ChargeRequest{
		Method:            item.PaymentMethod,
		AmountCents:       item.TotalCents,
		Currency:          item.Currency,
		Description:       describe(item),
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
	// Pago: a few hundred milliseconds normally, and up to the whole retry
	// budget when the provider is failing, and the order can move during it:
	// the hold expires and the tickets go back on sale, a webhook settles it,
	// an admin cancels it. Writing this stale copy back would silently undo
	// that, resurrecting an expired order to pending_payment while its stock
	// belongs to somebody else. settle re-reads the row under FOR UPDATE and
	// is the only place that writes it.
	//
	// It also covers the charge that is born final, an already-approved
	// payment, or one the provider rejects outright, which must be settled
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
// reference, which covers the genuine race where the provider's notification
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
// timeout left the money refunded at Mercado Pago and the order untouched here:
// the two facts that must never disagree, disagreeing, with nothing left to
// reconcile them. The job row is durable, retried with backoff, and keyed on
// the order, so an operator double-clicking refunds once.
//
// What IS checked here is everything a person should learn immediately: an
// order that does not exist, or one there is nothing to refund on. Those would
// only ever park a job and wait for someone to read the log.
func (s *Service) RequestRefund(
	ctx context.Context,
	actor authdomain.Actor,
	orderID string,
) (*orderdomain.Order, error) {
	// Refunds move money OUT, so only an operator may ask for one. Enforced
	// here rather than in the HTTP handler: this method is also reachable from
	// the refund use case, and a rule that only the handler applied would be a
	// rule that path skipped.
	if !actor.IsAdmin() {
		return nil, fmt.Errorf("%w: only an operator may refund an order", authdomain.ErrForbidden)
	}
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

	job, err := s.EnqueueRefund(ctx, s.jobs, item)
	if err != nil {
		return nil, err
	}
	s.dispatcher.Dispatch(ctx, job)
	return item, nil
}

// EnqueueRefund schedules the money movement for one order onto the queue it is
// given, and reports the job so the caller can announce it after committing.
//
// The queue is a PARAMETER rather than this service's own, and that is the
// whole reason this is exported. A refund approved by an organiser has to write
// the approval and schedule the money in one transaction: an approval nobody
// acts on, or money leaving with no record of who allowed it, are both states
// this pair can reach if they commit separately. So usecases/refund passes the
// queue bound to its unit of work, and the ordinary admin path above passes
// this service's own.
//
// Duplicating the job shape at that call site was the alternative, and the
// dedupe key is exactly the thing that must not be written twice: it is what
// makes an operator double-clicking refund once.
func (s *Service) EnqueueRefund(
	ctx context.Context,
	jobs queue.Queue,
	item *orderdomain.Order,
) (*queue.Job, error) {
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
	added, err := jobs.Enqueue(ctx, job)
	if err != nil {
		return nil, err
	}
	if !added {
		// Already scheduled; nothing new to publish.
		return nil, nil
	}
	return job, nil
}

// CancelCharge voids the charge of an order whose hold has lapsed. It is the
// handler behind TypeCancelCharge.
//
// WHAT THIS PREVENTS: a hold is thirty minutes and a provider dates a charge to
// a day, so releasing an order's stock used to leave its PIX code payable for
// another twenty-odd hours. A buyer who paid in that window sent real money for
// seats that were already back on sale and usually already sold, and the box
// office's only remaining move was to take the money and give it straight back,
// losing the provider's fee and disappointing somebody who thought they had
// tickets. Cancelling closes the window to the length of the hold.
//
// Deliberately gentle about every way it can find nothing to do. This runs for
// every expired hold on the system, which is the ordinary outcome of an
// abandoned cart, so "there was nothing to cancel" is the common case and not
// an error worth a retry or a log line.
func (s *Service) CancelCharge(ctx context.Context, orderID string) error {
	item, err := s.orders.GetByID(ctx, orderID)
	if errors.Is(err, orderdomain.ErrNotFound) {
		return queue.Permanent(fmt.Errorf("cancel charge for order %s: %w", orderID, err))
	}
	if err != nil {
		return err
	}
	if item.PaymentID == "" {
		// The hold lapsed before the charge was ever created, which is most
		// abandoned carts: nothing was issued, so nothing is payable.
		return nil
	}
	if s.gateway == nil {
		return paymentdomain.ErrNotConfigured
	}
	// The order has to still be one nobody may pay for.
	//
	// Checked at the moment of cancelling rather than trusted from the job,
	// because the job was written when the hold lapsed and the world moves: a
	// buyer who paid in the seconds between then and now has a PAID order, and
	// voiding their charge would take away tickets they legitimately hold. The
	// race is real, it is why this is a status check and not a flag.
	if item.Status != orderdomain.StatusExpired && item.Status != orderdomain.StatusCancelled {
		return nil
	}
	if err := s.gateway.CancelCharge(ctx, item.PaymentID); err != nil {
		if !paymentdomain.Retryable(err) {
			// A provider that refuses to void is almost always telling us the
			// charge was already paid, and settlement is the thing that
			// handles that, through reconciliation, exactly as it did before
			// this existed. Parked rather than retried, and not an alarm.
			log.Printf("payment: charge %s for expired order %s could not be voided: %v", item.PaymentID, orderID, err)
			return queue.Permanent(fmt.Errorf("cancel charge for order %s: %w", orderID, err))
		}
		return err
	}
	log.Printf("payment: voided charge %s for expired order %s", item.PaymentID, orderID)
	return nil
}

// EnqueueCancelCharge schedules the void, inside the caller's transaction.
//
// Written with the expiry that released the stock, so a code cannot stay
// payable for inventory that was given back; either both commit or neither
// does. Announced after the commit, like every other job here.
func (s *Service) EnqueueCancelCharge(
	ctx context.Context,
	jobs queue.Queue,
	item *orderdomain.Order,
) (*queue.Job, error) {
	if item == nil || item.PaymentID == "" {
		return nil, nil
	}
	payload, err := queue.NewPayload(queue.SyncPaymentPayload{OrderID: item.ID, PaymentID: item.PaymentID})
	if err != nil {
		return nil, err
	}
	job := &queue.Job{
		ID:          s.newID(),
		Type:        queue.TypeCancelCharge,
		Payload:     payload,
		RunAt:       s.now(),
		MaxAttempts: queue.DefaultMaxAttempts,
		// One void per order, however many sweeps notice the same lapsed hold.
		DedupeKey: queue.TypeCancelCharge + ":" + item.ID,
	}
	added, err := jobs.Enqueue(ctx, job)
	if err != nil {
		return nil, err
	}
	if !added {
		return nil, nil
	}
	return job, nil
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
		// Already given back: a redelivered job, or an operator who pressed
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
// order stays paid and the seat stays sold for good, the money went back and
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

// BackfillAdmissions issues the tickets that paid orders from before this
// feature existed never got.
//
// Admissions are minted in the transaction that crosses an order into paid, so
// an order that reached paid BEFORE the door feature shipped has none, and its
// buyer opens their wallet to "no tickets issued for this order yet" forever.
// That is every order paid before the deploy, which on a live box office is not
// a handful.
//
// It lives here, in the package that already owns "an order became paid,
// therefore tickets exist", and reuses the same issuing path settlement does.
// Nothing about it is special-cased: IssueForOrder is idempotent and reports
// only what IT created, so this is safe to run repeatedly, safe to run while
// settlements are happening, and cannot mint a second ticket for an order that
// already has one.
//
// Bounded by `limit` and paged from the oldest, so an operator can run it in
// slices rather than opening one transaction over a million rows.
func (s *Service) BackfillAdmissions(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	pageSize := limit
	if pageSize > 200 {
		pageSize = 200
	}

	issued := 0
	scanned := 0
	for scanned < limit {
		remaining := limit - scanned
		if remaining > pageSize {
			remaining = pageSize
		}
		paid, err := s.orders.List(ctx, orderdomain.Filter{
			Status: orderdomain.StatusPaid,
			Limit:  remaining,
			Offset: scanned,
			// Oldest first, and stable: the backfill never changes an order's
			// status, so the paid set does not shift under the paging and an
			// OFFSET walk cannot skip a row. It also means the orders whose
			// buyers have been waiting longest are served first.
			OldestUpdatedFirst: true,
		})
		if err != nil {
			return issued, err
		}
		if len(paid) == 0 {
			break
		}
		for index := range paid {
			item := &paid[index]
			// One transaction per order. A single transaction over the whole
			// batch would hold locks on every one of them while it ran, and a
			// failure in the last order would discard the tickets minted for
			// all the others.
			err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
				minted, err := issueAdmissions(ctx, repositories, item)
				if err != nil {
					return err
				}
				issued += minted
				return nil
			})
			if err != nil {
				return issued, err
			}
		}
		scanned += len(paid)
		if len(paid) < remaining {
			break
		}
	}
	return issued, nil
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
			Status: orderdomain.StatusPendingPayment,
			// Only orders nobody has looked at recently. Settlement refreshes
			// UpdatedAt even when the charge has not moved, so each read
			// pushes the next one out by this much and the backoff needs no
			// state of its own. Without it this sweep is a poller, and the
			// providers price polling: Asaas caps an account at 25,000 calls
			// per twelve hours and declines to raise it for accounts that
			// poll.
			UpdatedBefore:      s.now().Add(-s.reconcileAfter),
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
// issueAdmissions mints one credential per ticket on a newly paid order.
//
// Lives beside settle rather than in its own use case because it is part of
// what "paid" means here, and because it must run inside the caller's
// transaction: the admissions and the paid status commit together or neither
// does.
func issueAdmissions(ctx context.Context, repositories uow.Repositories, item *orderdomain.Order) (int, error) {
	lines := make([]admissiondomain.Line, 0, len(item.Items))
	for _, line := range item.Items {
		lines = append(lines, admissiondomain.Line{
			TicketID:    line.TicketID,
			TicketTitle: line.TicketTitle,
			Quantity:    line.Quantity,
			// A seated line is one chair and Quantity 1, so this mints one
			// admission carrying that chair's label. A counted line carries no
			// seat and mints Quantity of them, exactly as before.
			SeatID: line.SeatID,
			Seat:   line.Seat,
		})
	}

	issued, err := repositories.Admissions().IssueForOrder(ctx, admissiondomain.OrderLines{
		OrderID: item.ID,
		EventID: item.EventID,
		Lines:   lines,
	})
	if err != nil {
		return 0, fmt.Errorf("issue admissions for order %s: %w", item.ID, err)
	}
	if len(issued) > 0 {
		// One line per order, not per ticket: a festival order for twenty
		// would otherwise be twenty lines of log for one sale. The codes
		// themselves are never logged.
		log.Printf("admission: issued %d admission(s) for order %s at event %s",
			len(issued), item.ID, item.EventID)
	}
	return len(issued), nil
}

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
		// paid order the first committed, the redelivery case handled by the
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
			// knows; the provider's answer to "create the charge" overtaken
			// by the webhook that settled it, and older news is not recorded
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
		// cannot legally move out of, and the honest answer, "we owe a
		// refund", would be unreachable.
		reservedLate := false
		if next == orderdomain.StatusPaid && !previous.HoldsStock() {
			// The hold lapsed and the tickets went back on sale. Paying for an
			// expired reservation is frequent: a buyer opens the bank app,
			// gets distracted, pays eleven minutes later, so the stock is
			// asked for again rather than assumed.
			reserved, err := reserveAll(ctx, repositories, item)
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
				if releaseErr := releaseAll(ctx, repositories, item); releaseErr != nil {
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
				if releaseErr := releaseAll(ctx, repositories, item); releaseErr != nil {
					return releaseErr
				}
			}
			return repositories.Orders().Update(ctx, item)
		}

		switch next {
		case orderdomain.StatusPaid:
			// Either the hold this order already had, or the one just taken
			// back for it.
			for _, line := range item.Items {
				if err := repositories.Tickets().Commit(ctx, line.TicketID, line.Quantity); err != nil {
					return err
				}
			}
			// The chairs move with the counters, in the same transaction. A
			// paid order whose seats stayed "held" would have them swept back
			// onto sale by the lapsed-hold job minutes later, and then sold to
			// somebody else while the first buyer holds a valid ticket for
			// them.
			if _, err := repositories.Seats().CommitForOrder(ctx, item.ID); err != nil {
				return err
			}
			// And the tickets themselves, in the same transaction that took
			// the money. A paid order with no admissions is a buyer holding a
			// receipt and no way through the door; issuing them afterwards, in
			// a job, would leave a window where exactly that is true.
			//
			// `changed` above already guarantees one crossing into paid, and
			// the repository is idempotent besides, so a redelivered webhook
			// cannot mint a second set.
			if _, err := issueAdmissions(ctx, repositories, item); err != nil {
				return err
			}
			// And what the organiser is now owed, in the same transaction and
			// for the same reason. A balance that can disagree with the
			// payments behind it is one somebody reconciles by hand forever.
			if s.ledger != nil {
				if err := s.ledger.Accrue(ctx, repositories, item); err != nil {
					return err
				}
			}
		case orderdomain.StatusRefundRequired:
			// The money is ours and should not be. Give it back here, in the
			// transaction that recorded the debt, so there is no window in
			// which the system knows it owes a buyer and has done nothing
			// about it. Before this, refund_required was written and that was
			// the end of it: no request, no job, one log line, and the money
			// stayed until somebody thought to query for the status.
			//
			// No stock is touched. The seats are gone precisely because
			// somebody else bought them, which is what made this the outcome.
			if s.refunds != nil {
				job, err := s.refunds.OpenOperatorRefund(ctx, repositories, item)
				if err != nil {
					return err
				}
				if job != nil {
					messages = append(messages, job)
				}
			}
		case orderdomain.StatusExpired, orderdomain.StatusFailed, orderdomain.StatusCancelled:
			if previous.HoldsStock() {
				if err := releaseAll(ctx, repositories, item); err != nil {
					return err
				}
			}
		case orderdomain.StatusRefunded:
			// Only stock that was actually sold comes back; refunding an order
			// that never completed would credit inventory already released.
			if previous == orderdomain.StatusPaid {
				for _, line := range item.Items {
					if err := repositories.Tickets().ReleaseSold(ctx, line.TicketID, line.Quantity); err != nil {
						return err
					}
				}
				// The refunded chair goes back on sale too. Scoped to sold, the
				// mirror of the release above: between them a seat can only
				// leave an order the same way it arrived.
				if _, err := repositories.Seats().ReleaseSoldForOrder(ctx, item.ID); err != nil {
					return err
				}
			}
			// The money went back, so the entry goes with it. Withdrawn rather
			// than deleted, so a holder who turns up with a refunded ticket is
			// told it was refunded instead of being told it never existed.
			voided, err := repositories.Admissions().VoidForOrder(ctx, item.ID)
			if err != nil {
				return err
			}
			if voided > 0 {
				log.Printf("admission: voided %d admission(s) for refunded order %s", voided, item.ID)
			}
			// The organiser's claim goes back with the money. Available
			// immediately, unlike the sale it reverses, which is what stops a
			// payout run paying out a ticket that has already been refunded.
			//
			// The KIND follows the charge, not the order. Both a refund and a
			// chargeback move an order to `refunded`: money left, and the
			// order's own vocabulary has one word for that, but they are not
			// the same event to the organiser or to us. A refund is a decision
			// somebody made; a chargeback is a bank pulling money back under
			// Pix's Mecanismo Especial de Devolução, which is what the reserve
			// exists for and which the Terms bill differently. Recording both
			// as a refund loses that distinction on the statement of the person
			// whose money it is.
			if s.ledger != nil {
				kind := ledgerdomain.KindRefund
				if charge.Status == paymentdomain.StatusChargedBack {
					kind = ledgerdomain.KindChargeback
				}
				if err := s.ledger.Reverse(ctx, repositories, item, kind); err != nil {
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
// The tier is read for the event details the message carries; the show, the
// door time, the venue, because a receipt that says only "R$ 240,00" is not a
// receipt. A tier that has been deleted costs those details and not the
// settlement: the message still goes out, naming the order.
func (s *Service) raise(
	ctx context.Context,
	repositories uow.Repositories,
	item *orderdomain.Order,
	compose func(context.Context, queue.Queue, *orderdomain.Order, *ticketdomain.Ticket, *eventdomain.Event) (*queue.Job, error),
) (*queue.Job, error) {
	if s.notifications == nil {
		// No provider configured: skip the read as well as the message.
		return nil, nil
	}
	// The event carries what the receipt actually reads as: the show, the door
	// time, the venue. Read straight off the order, which now names the night
	// it is for, the tiers underneath it only ever agreed about that anyway.
	var happening *eventdomain.Event
	if item.EventID != "" {
		found, err := repositories.Events().GetByID(ctx, item.EventID)
		if err != nil && !errors.Is(err, eventdomain.ErrNotFound) {
			return nil, err
		}
		happening = found
	}
	// The tier is still read for a single-line order, because a receipt for one
	// tier can name it in the subject. A multi-line order has no single tier to
	// name and the message lists the lines instead.
	var tier *ticketdomain.Ticket
	if len(item.Items) == 1 {
		found, err := repositories.Tickets().GetByID(ctx, item.Items[0].TicketID)
		if err != nil && !errors.Is(err, ticketdomain.ErrNotFound) {
			return nil, err
		}
		tier = found
	}
	return compose(ctx, repositories.Jobs(), item, tier, happening)
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

// reserveAll takes stock back for every line of a late-paid order, or takes
// none at all.
//
// All or nothing is the only honest answer. An order half-reserved is an order
// the buyer paid for in full and would be admitted on in part, and there is no
// status that describes that, so a line that cannot be re-taken releases the
// lines above it and the whole order becomes a refund the box office owes.
//
// The lines are walked in the order they are held, which NormalizeItems sorted
// by tier id at checkout. Two settlements racing over the same two tiers
// therefore lock them in the same sequence and cannot deadlock.
func reserveAll(ctx context.Context, repositories uow.Repositories, item *orderdomain.Order) (bool, error) {
	// The chairs first, and NOT the same chairs-or-nothing question the
	// counters ask.
	//
	// A counted line can be satisfied by any stock: the buyer asked for two
	// Pista and two Pista is two Pista. A seated line cannot. This buyer paid
	// for FILA K POLTRONA 12, it is printed on the ticket they already have,
	// and if somebody else took it while the hold was lapsed then there is no
	// substitute this function is allowed to choose for them. So the claim asks
	// for exactly the seats the order names, and failing it is what sends the
	// order to refund_required rather than to a different chair.
	if seated, err := reclaimSeats(ctx, repositories, item); err != nil {
		return false, err
	} else if !seated {
		return false, nil
	}

	taken := make([]orderdomain.Item, 0, len(item.Items))
	for _, line := range item.Items {
		reserved, err := repositories.Tickets().Reserve(ctx, line.TicketID, line.Quantity)
		if err != nil {
			return false, err
		}
		if reserved {
			taken = append(taken, line)
			continue
		}
		for _, undo := range taken {
			if err := repositories.Tickets().Release(ctx, undo.TicketID, undo.Quantity); err != nil {
				return false, err
			}
		}
		// The seats go back with the counters. Without this a basket that lost
		// its counted stock would keep holding chairs nobody can buy, for an
		// order that is about to be marked refund_required.
		if _, err := repositories.Seats().ReleaseForOrder(ctx, item.ID); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// reclaimSeats takes back exactly the chairs an order already names.
//
// Reached only when a payment arrived after the hold lapsed, which released
// them. It reports false, not an error, when any one of them is gone, because
// that is the honest, frequent case the caller turns into "we owe a refund".
//
// Returns true immediately for a counted order, which is every order of a
// general-admission event.
func reclaimSeats(ctx context.Context, repositories uow.Repositories, item *orderdomain.Order) (bool, error) {
	byTier := make(map[string][]string)
	for _, line := range item.Items {
		if line.Seated() {
			byTier[line.TicketID] = append(byTier[line.TicketID], line.SeatID)
		}
	}
	if len(byTier) == 0 {
		return true, nil
	}

	// Sorted, so two settlements of two orders over the same tiers take the
	// seat locks in the same order and cannot deadlock. The tier ids are sorted
	// here and the seat ids inside ClaimRequest.Validate.
	tiers := make([]string, 0, len(byTier))
	for tierID := range byTier {
		tiers = append(tiers, tierID)
	}
	sort.Strings(tiers)

	for _, tierID := range tiers {
		result, err := repositories.Seats().Claim(ctx, seatingdomain.ClaimRequest{
			EventID:  item.EventID,
			TicketID: tierID,
			OrderID:  item.ID,
			SeatIDs:  byTier[tierID],
			// The order's own deadline, already past. It is written only to
			// satisfy the not-null guard on a held seat, and the commit that
			// follows in this same transaction clears it a statement later.
			HoldExpiresAt: item.HoldExpiresAt,
		})
		if err != nil {
			return false, err
		}
		if !result.OK() {
			// Give back whatever this loop did manage to take, so the order
			// about to become refund_required is not sitting on chairs.
			if _, releaseErr := repositories.Seats().ReleaseForOrder(ctx, item.ID); releaseErr != nil {
				return false, releaseErr
			}
			log.Printf("payment: order %s paid after its hold expired and %d of its seats are gone; refund required",
				item.ID, len(result.Unavailable))
			return false, nil
		}
	}
	return true, nil
}

// releaseAll returns every line's held stock, and the chairs with it.
func releaseAll(ctx context.Context, repositories uow.Repositories, item *orderdomain.Order) error {
	for _, line := range item.Items {
		if err := repositories.Tickets().Release(ctx, line.TicketID, line.Quantity); err != nil {
			return err
		}
	}
	// Keyed by the order, not by the line: a seat is held by whoever holds it.
	// Scoped to held inside the repository, so a sweep arriving after a
	// settlement cannot put a paid chair back on sale.
	_, err := repositories.Seats().ReleaseForOrder(ctx, item.ID)
	return err
}

// describe is what the buyer will see on their bank statement.
//
// The tier titles rather than their ids, because "Ingresso tkt_9f2c1a x2" is
// what a chargeback looks like: nobody recognises it, so they dispute it.
func describe(item *orderdomain.Order) string {
	if len(item.Items) == 0 {
		return "Ingressos"
	}
	parts := make([]string, 0, len(item.Items))
	for _, line := range item.Items {
		title := line.TicketTitle
		if title == "" {
			title = line.TicketID
		}
		parts = append(parts, fmt.Sprintf("%dx %s", line.Quantity, title))
	}
	return "Ingressos: " + strings.Join(parts, ", ")
}
