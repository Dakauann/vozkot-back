// Package checkout turns "I want three Pista and one Camarote" into a held
// order waiting for money.
//
// Stock and the order are written in ONE transaction. A reservation without an
// order is inventory nobody can claim; an order without a reservation is a
// promise the box office cannot keep. Committing them together is what makes
// both impossible.
//
// The hold happens in two phases, and that is a deliberate departure from the
// obvious design:
//
//   - RESERVE, the moment a buyer reaches the details form. The tickets come
//     off the shelf immediately, for a short window. Waiting until the form is
//     submitted means every buyer fills in their name, their email and their
//     CPF against stock anyone can still take, and learns at the last keystroke
//     that it is gone.
//   - CONFIRM, when they submit it. The window extends to the full payment
//     window and the charge is requested.
//
// One window for both would have to be the long one, and a long window opened
// by merely visiting a page is how an event gets held hostage by a crawler.
package checkout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	orderdomain "vozkot/domain/order"
	"vozkot/domain/payment"
	"vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/domain/uow"
	queueUsecase "vozkot/usecases/queue"
)

// DefaultHoldFor is how long a buyer has to pay once they have confirmed.
//
// Thirty minutes, and not by taste: Mercado Pago refuses a PIX charge that
// expires sooner, so a ten-minute hold would issue a code that outlives its own
// reservation on every order — turning the late-payment path from an edge case
// into the normal one. The hold and the charge expire together.
const DefaultHoldFor = 30 * time.Minute

// DefaultCartHoldFor is how long the tickets stay off the shelf while the buyer
// fills in the form.
//
// Ten minutes, which is generous for typing a name and an email and short
// enough that an abandoned basket is back on sale before anyone notices it
// left. It is deliberately NOT the payment window: nothing has been asked of
// the buyer yet, and stock held on the strength of a page view should cost the
// event as little as possible.
const DefaultCartHoldFor = 10 * time.Minute

type Service struct {
	unit uow.Runner
	// dispatcher announces committed jobs to the broker. Nil is supported and
	// means the poller is the only delivery path.
	dispatcher  *queueUsecase.Dispatcher
	orders      orderdomain.Repository
	tickets     ticketdomain.Repository
	holdFor     time.Duration
	cartHoldFor time.Duration
	// holdLimits caps what one account may keep reserved and unpaid at once.
	// The zero value caps nothing, which is what the load harness wants.
	holdLimits orderdomain.HoldLimits
	now        func() time.Time
	newID      func() string
}

func NewService(
	unit uow.Runner,
	orders orderdomain.Repository,
	tickets ticketdomain.Repository,
	dispatcher *queueUsecase.Dispatcher,
	holdFor time.Duration,
	cartHoldFor time.Duration,
	holdLimits orderdomain.HoldLimits,
) *Service {
	if holdFor <= 0 {
		holdFor = DefaultHoldFor
	}
	if cartHoldFor <= 0 {
		cartHoldFor = DefaultCartHoldFor
	}
	if cartHoldFor > holdFor {
		// A cart window longer than the payment window would mean confirming an
		// order SHORTENED its hold, which is the opposite of what confirming
		// is for. Clamping is better than refusing to start over a
		// misconfiguration with an obvious right answer.
		cartHoldFor = holdFor
	}
	return &Service{
		unit:        unit,
		dispatcher:  dispatcher,
		orders:      orders,
		tickets:     tickets,
		holdFor:     holdFor,
		cartHoldFor: cartHoldFor,
		holdLimits:  holdLimits,
		now:         time.Now,
		newID:       randomID,
	}
}

// StartInput is one purchase attempt: one event, one or more of its tiers.
type StartInput struct {
	Items         []orderdomain.DraftItem
	BuyerID       string
	BuyerName     string
	BuyerEmail    string
	BuyerDocument string
	Method        payment.Method
	// IdempotencyKey is the client's retry token. The HTTP layer enforces the
	// replay contract; the order keeps the key so an order can always be traced
	// back to the request that made it.
	IdempotencyKey string
	// Confirm collapses both phases into one call: reserve and immediately ask
	// for the charge. It exists for callers that have the buyer's details
	// already — an operator selling at the door, and the load harness — and not
	// for the browser flow, which always confirms as its own step.
	Confirm bool
}

// Start reserves stock for every line and opens an order.
//
// The charge itself is NOT created here, and on the browser path it is not
// created by this call at all. Asking a payment provider for a PIX code is a
// round trip to a third party that is occasionally slow and occasionally down,
// and a buyer holding a reservation should not wait on it — nor should the
// reservation be lost because the provider timed out. The job enqueued in this
// same transaction owns that call, which is the outbox pattern: the work cannot
// be scheduled unless the order it refers to was committed, and cannot be lost
// if it was.
func (s *Service) Start(ctx context.Context, input StartInput) (*orderdomain.Order, error) {
	requested, err := orderdomain.NormalizeItems(input.Items)
	if err != nil {
		return nil, err
	}

	var created *orderdomain.Order
	var announce []*queue.Job

	err = s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		// Read every tier first, before anything is reserved. The prices, the
		// titles and the event all come from these rows and never from the
		// request: a client that names its own price names nothing.
		lines := make([]orderdomain.Item, 0, len(requested))
		eventID := ""
		currency := ""
		for _, wanted := range requested {
			tier, err := repositories.Tickets().GetByID(ctx, wanted.TicketID)
			if err != nil {
				return err
			}
			if eventID == "" {
				eventID = tier.EventID
			} else if tier.EventID != eventID {
				// One order is one night. Two events in a basket would share a
				// hold window, a PIX code and a receipt that could only name
				// one venue.
				return orderdomain.ErrMultipleEvents
			}
			if currency == "" {
				currency = tier.Currency
			}
			// Advisory, for a clear error message. The authority is the
			// conditional update below, which is the only check two concurrent
			// buyers cannot both pass.
			if err := tier.CanReserve(wanted.Quantity); err != nil {
				return err
			}
			lines = append(lines, orderdomain.Item{
				ID:             s.newItemID(),
				TicketID:       tier.ID,
				TicketTitle:    tier.Title,
				Quantity:       wanted.Quantity,
				UnitPriceCents: tier.PriceCents,
			})
		}

		// Before any inventory moves: is this account already sitting on more
		// than it is allowed to? The check is inside the transaction and behind
		// a per-buyer lock, so two simultaneous checkouts cannot both pass it.
		//
		// It runs BEFORE the reservations so the lock order is always
		// buyer-then-tiers. Taking them the other way round in some other path
		// would be the classic deadlock; there is only one path, and this is
		// it.
		if err := s.withinHoldLimits(ctx, repositories, input.BuyerID, lines); err != nil {
			return err
		}

		// In the order NormalizeItems sorted them into, which is what keeps two
		// orders over the same two tiers from deadlocking against each other.
		for _, line := range lines {
			reserved, err := repositories.Tickets().Reserve(ctx, line.TicketID, line.Quantity)
			if err != nil {
				return err
			}
			if !reserved {
				// Someone took the last ones between the read and the update.
				// This is the race the conditional update exists to lose
				// safely, and losing it rolls the whole transaction back —
				// including every line already reserved above, which is why a
				// partial basket can never be committed.
				return ticketdomain.ErrInsufficientStock
			}
		}

		holdFor := s.cartHoldFor
		if input.Confirm {
			holdFor = s.holdFor
		}
		item, err := orderdomain.New(s.newID(), orderdomain.Draft{
			EventID:        eventID,
			BuyerID:        input.BuyerID,
			BuyerName:      input.BuyerName,
			BuyerEmail:     input.BuyerEmail,
			BuyerDocument:  input.BuyerDocument,
			Items:          lines,
			Currency:       currency,
			Method:         input.Method,
			IdempotencyKey: input.IdempotencyKey,
		}, holdFor, s.now())
		if err != nil {
			return err
		}
		if input.Confirm {
			if _, err := item.Confirm(
				input.BuyerName, input.BuyerEmail, input.BuyerDocument,
				input.Method, s.holdFor, s.now(),
			); err != nil {
				return err
			}
		}
		if err := repositories.Orders().Create(ctx, item); err != nil {
			return err
		}

		if input.Confirm {
			charge, err := s.enqueueCharge(ctx, repositories.Jobs(), item)
			if err != nil {
				return err
			}
			if charge != nil {
				announce = append(announce, charge)
			}
		}
		// The hold's expiry is scheduled as its own job rather than left to the
		// sweep, so stock comes back the moment it is due even on a quiet
		// system. The sweep stays as the safety net.
		if _, err := s.enqueueExpiry(ctx, repositories.Jobs(), item); err != nil {
			return err
		}

		created = item
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Announced only now: publishing inside the transaction would tell a
	// consumer about an order that might still roll back, and consumers are
	// fast enough to look for it before it exists.
	s.dispatcher.Dispatch(ctx, announce...)
	return created, nil
}

// ConfirmInput is the details step: who is buying, and how they will pay.
type ConfirmInput struct {
	OrderID string
	// BuyerID is the signed-in account. An order may only be confirmed by the
	// account that opened it, checked here rather than only at the edge so no
	// future caller can skip it.
	BuyerID       string
	BuyerName     string
	BuyerEmail    string
	BuyerDocument string
	Method        payment.Method
}

// Confirm accepts the buyer's details, extends the hold to the payment window
// and asks for the charge.
//
// Safe to call twice. The second call saves any corrected details, does not
// extend the window again, and does not create a second charge — the job's
// dedupe key sees to the last of those even if this method's own guard were
// ever removed.
func (s *Service) Confirm(ctx context.Context, input ConfirmInput) (*orderdomain.Order, error) {
	var result *orderdomain.Order
	var chargeJob *queue.Job

	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		item, err := repositories.Orders().GetByIDForUpdate(ctx, input.OrderID)
		if err != nil {
			return err
		}
		if item.BuyerID != "" && input.BuyerID != "" && item.BuyerID != input.BuyerID {
			return orderdomain.ErrNotFound
		}
		if item.HoldLapsed(s.now()) {
			// The window closed while the form was open. Say so rather than
			// extending a hold whose stock the sweep is about to return.
			return orderdomain.ErrHoldExpired
		}

		extended, err := item.Confirm(
			input.BuyerName, input.BuyerEmail, input.BuyerDocument,
			input.Method, s.holdFor, s.now(),
		)
		if err != nil {
			return err
		}
		if err := repositories.Orders().Update(ctx, item); err != nil {
			return err
		}
		result = item
		if !extended {
			// Already confirmed. The details were saved above; nothing else
			// about this call is new.
			return nil
		}

		charge, err := s.enqueueCharge(ctx, repositories.Jobs(), item)
		if err != nil {
			return err
		}
		chargeJob = charge
		// The hold now ends later than the job scheduled at reserve time. That
		// job will fire, find the hold still standing and do nothing; this one
		// is the one that will actually expire it.
		if _, err := s.enqueueExpiry(ctx, repositories.Jobs(), item); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.dispatcher.Dispatch(ctx, chargeJob)
	return result, nil
}

// withinHoldLimits refuses a buyer already holding more unpaid inventory than
// the box office allows.
//
// This is what closes the arithmetic the per-minute rate limit leaves open. At
// thirty checkouts a minute, ten tickets each and a thirty-minute hold, an
// account that merely cycles can keep nine thousand tickets off the shelf with
// no money at risk — the rate limit bounds how fast someone reserves, never how
// much they are sitting on, and it is the second number that empties an event.
func (s *Service) withinHoldLimits(
	ctx context.Context,
	repositories uow.Repositories,
	buyerID string,
	lines []orderdomain.Item,
) error {
	if s.holdLimits.Unlimited() || buyerID == "" {
		// No limits configured, or a sale with no account behind it: nothing to
		// count against, and no reason to pay for the lock.
		return nil
	}
	tiers := make([]string, 0, len(lines))
	for _, line := range lines {
		tiers = append(tiers, line.TicketID)
	}
	current, err := repositories.Orders().CountOpenHoldsForUpdate(ctx, buyerID, tiers)
	if err != nil {
		return err
	}
	return s.holdLimits.Allows(current, lines)
}

func (s *Service) Get(ctx context.Context, id string) (*orderdomain.Order, error) {
	return s.orders.GetByID(ctx, id)
}

// FindByIdempotencyKey returns the order a checkout key produced, if it
// produced one.
//
// It exists for recovery: the idempotency claim is written before the checkout
// and completed after it, so a process killed between the two leaves a real
// order behind a key that looks unstarted. The buyer's retry has to be able to
// find that order and be shown it, rather than reserving a second batch of
// tickets they never asked for.
func (s *Service) FindByIdempotencyKey(ctx context.Context, key string) (*orderdomain.Order, error) {
	return s.orders.FindByIdempotencyKey(ctx, key)
}

// Page is a listing plus its total.
type Page struct {
	Items []orderdomain.Order
	Total int64
}

func (s *Service) List(ctx context.Context, filter orderdomain.Filter) (Page, error) {
	items, err := s.orders.List(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	total, err := s.orders.Count(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	return Page{Items: items, Total: total}, nil
}

// Cancel gives up a pending order and returns its stock.
//
// Cancelling an order that is already cancelled succeeds and changes nothing: a
// buyer who taps the button twice has not done anything wrong.
func (s *Service) Cancel(ctx context.Context, id string) (*orderdomain.Order, error) {
	var result *orderdomain.Order

	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		item, err := repositories.Orders().GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}
		heldStock := item.Status.HoldsStock()

		changed, err := item.Apply(orderdomain.StatusCancelled, s.now())
		if err != nil {
			return err
		}
		result = item
		if !changed {
			return nil
		}
		if err := repositories.Orders().Update(ctx, item); err != nil {
			return err
		}
		if heldStock {
			return releaseAll(ctx, repositories, item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ExpireHolds returns the stock of every order whose window has passed.
//
// Claim-then-release, never read-then-release: the repository flips the rows to
// expired in one statement and hands back only what it actually flipped, so two
// sweepers running at once split the work instead of both releasing the same
// tickets.
func (s *Service) ExpireHolds(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	released := 0

	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		expired, err := repositories.Orders().ClaimExpired(ctx, s.now(), limit)
		if err != nil {
			return err
		}
		for index := range expired {
			item := expired[index]
			if err := releaseAll(ctx, repositories, &item); err != nil {
				return err
			}
			released++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return released, nil
}

// ExpireHold releases one order's hold, driven by the job scheduled at its
// expiry. It is a no-op for an order that has since been paid, cancelled, or
// had its window extended by a confirmation — which is the common case on a
// healthy system, because every order schedules one of these at cart time and
// most of them go on to be confirmed.
func (s *Service) ExpireHold(ctx context.Context, orderID string) error {
	return s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		item, err := repositories.Orders().GetByIDForUpdate(ctx, orderID)
		if errors.Is(err, orderdomain.ErrNotFound) {
			// Nothing to expire, and nothing a retry could find.
			return queue.Permanent(fmt.Errorf("expire hold for order %s: %w", orderID, err))
		}
		if err != nil {
			return err
		}
		if !item.HoldLapsed(s.now()) {
			return nil
		}
		changed, err := item.Apply(orderdomain.StatusExpired, s.now())
		if err != nil || !changed {
			return err
		}
		if err := repositories.Orders().Update(ctx, item); err != nil {
			return err
		}
		return releaseAll(ctx, repositories, item)
	})
}

// releaseAll returns every line's stock, in the order the items are held.
//
// Same order as the reservation took them, for the same reason: two
// transactions touching the same two tiers must agree on which to lock first.
func releaseAll(ctx context.Context, repositories uow.Repositories, item *orderdomain.Order) error {
	for _, line := range item.Items {
		if err := repositories.Tickets().Release(ctx, line.TicketID, line.Quantity); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) enqueueCharge(ctx context.Context, jobs queue.Queue, item *orderdomain.Order) (*queue.Job, error) {
	payload, err := queue.NewPayload(queue.CreateChargePayload{OrderID: item.ID})
	if err != nil {
		return nil, err
	}
	job := &queue.Job{
		ID:          s.newID(),
		Type:        queue.TypeCreateCharge,
		Payload:     payload,
		RunAt:       s.now(),
		MaxAttempts: queue.DefaultMaxAttempts,
		// One charge per order, however many times this is enqueued.
		DedupeKey: queue.TypeCreateCharge + ":" + item.ID,
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

// enqueueExpiry schedules the hold's own expiry. It is never published: the
// broker has no delayed delivery without a plugin, and a job due in ten or
// thirty minutes is exactly what the poller is for.
//
// The dedupe key carries the deadline, so the cart hold and the payment hold
// each get their own job. A key naming only the order would let the second
// enqueue collapse into the first, leaving the extended window with no job of
// its own and the sweep as its only backstop.
func (s *Service) enqueueExpiry(ctx context.Context, jobs queue.Queue, item *orderdomain.Order) (*queue.Job, error) {
	payload, err := queue.NewPayload(queue.SyncPaymentPayload{OrderID: item.ID})
	if err != nil {
		return nil, err
	}
	job := &queue.Job{
		ID:          s.newID(),
		Type:        queue.TypeExpireHolds,
		Payload:     payload,
		RunAt:       item.HoldExpiresAt,
		MaxAttempts: queue.DefaultMaxAttempts,
		DedupeKey: queue.TypeExpireHolds + ":" + item.ID + ":" +
			strconv.FormatInt(item.HoldExpiresAt.UTC().Unix(), 10),
	}
	if _, err := jobs.Enqueue(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func randomID() string {
	return prefixedID("ord_")
}

func (s *Service) newItemID() string {
	return prefixedID("oi_")
}

func prefixedID(prefix string) string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return prefix + time.Now().UTC().Format("20060102150405000000000")
	}
	return prefix + hex.EncodeToString(buffer)
}
