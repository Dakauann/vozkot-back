// Package checkout turns "I want three tickets" into a held order waiting for
// money.
//
// Stock and the order are written in ONE transaction. A reservation without an
// order is inventory nobody can claim; an order without a reservation is a
// promise the box office cannot keep. Committing them together is what makes
// both impossible.
package checkout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	orderdomain "vozkot/domain/order"
	"vozkot/domain/payment"
	"vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/domain/uow"
	queueUsecase "vozkot/usecases/queue"
)

// DefaultHoldFor is how long a buyer has to pay before the tickets go back.
//
// Thirty minutes, and not by taste: Mercado Pago refuses a PIX charge that
// expires sooner, so a ten-minute hold would issue a code that outlives its own
// reservation on every order — turning the late-payment path from an edge case
// into the normal one. The hold and the charge expire together.
const DefaultHoldFor = 30 * time.Minute

type Service struct {
	unit uow.Runner
	// dispatcher announces committed jobs to the broker. Nil is supported and
	// means the poller is the only delivery path.
	dispatcher *queueUsecase.Dispatcher
	orders     orderdomain.Repository
	tickets    ticketdomain.Repository
	holdFor    time.Duration
	now        func() time.Time
	newID      func() string
}

func NewService(
	unit uow.Runner,
	orders orderdomain.Repository,
	tickets ticketdomain.Repository,
	dispatcher *queueUsecase.Dispatcher,
	holdFor time.Duration,
) *Service {
	if holdFor <= 0 {
		holdFor = DefaultHoldFor
	}
	return &Service{
		unit:       unit,
		dispatcher: dispatcher,
		orders:     orders,
		tickets:    tickets,
		holdFor:    holdFor,
		now:        time.Now,
		newID:      randomID,
	}
}

// StartInput is one purchase attempt.
type StartInput struct {
	TicketID      string
	Quantity      int
	BuyerID       string
	BuyerName     string
	BuyerEmail    string
	BuyerDocument string
	Method        payment.Method
	// IdempotencyKey is the client's retry token. The HTTP layer enforces the
	// replay contract; the order keeps the key so an order can always be traced
	// back to the request that made it.
	IdempotencyKey string
}

// Start reserves stock and opens an order.
//
// The charge itself is NOT created here. Asking a payment provider for a PIX
// code is a round trip to a third party that is occasionally slow and
// occasionally down, and a buyer holding a reservation should not wait on it —
// nor should the reservation be lost because the provider timed out. The job
// enqueued in this same transaction owns that call, which is the outbox
// pattern: the work cannot be scheduled unless the order it refers to was
// committed, and cannot be lost if it was.
func (s *Service) Start(ctx context.Context, input StartInput) (*orderdomain.Order, error) {
	var created *orderdomain.Order
	var chargeJob *queue.Job

	err := s.unit.Run(ctx, func(ctx context.Context, repositories uow.Repositories) error {
		ticket, err := repositories.Tickets().GetByID(ctx, input.TicketID)
		if err != nil {
			return err
		}
		// Advisory, for a clear error message. The authority is the conditional
		// update below, which is the only check two concurrent buyers cannot
		// both pass.
		if err := ticket.CanReserve(input.Quantity); err != nil {
			return err
		}

		reserved, err := repositories.Tickets().Reserve(ctx, ticket.ID, input.Quantity)
		if err != nil {
			return err
		}
		if !reserved {
			// Someone took the last ones between the read and the update. This
			// is the race the conditional update exists to lose safely.
			return ticketdomain.ErrInsufficientStock
		}

		item, err := orderdomain.New(s.newID(), orderdomain.Draft{
			TicketID:       ticket.ID,
			BuyerID:        input.BuyerID,
			BuyerName:      input.BuyerName,
			BuyerEmail:     input.BuyerEmail,
			BuyerDocument:  input.BuyerDocument,
			Quantity:       input.Quantity,
			UnitPriceCents: ticket.PriceCents,
			Currency:       ticket.Currency,
			Method:         input.Method,
			IdempotencyKey: input.IdempotencyKey,
		}, s.holdFor, s.now())
		if err != nil {
			return err
		}
		if err := repositories.Orders().Create(ctx, item); err != nil {
			return err
		}
		charge, err := s.enqueueCharge(ctx, repositories.Jobs(), item)
		if err != nil {
			return err
		}
		chargeJob = charge
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
	s.dispatcher.Dispatch(ctx, chargeJob)
	return created, nil
}

func (s *Service) Get(ctx context.Context, id string) (*orderdomain.Order, error) {
	return s.orders.GetByID(ctx, id)
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
			return repositories.Tickets().Release(ctx, item.TicketID, item.Quantity)
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
			if err := repositories.Tickets().Release(ctx, item.TicketID, item.Quantity); err != nil {
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
// expiry. It is a no-op for an order that has since been paid or cancelled,
// which is the common case on a healthy system.
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
		return repositories.Tickets().Release(ctx, item.TicketID, item.Quantity)
	})
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
// broker has no delayed delivery without a plugin, and a job due in thirty
// minutes is exactly what the poller is for.
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
		DedupeKey:   queue.TypeExpireHolds + ":" + item.ID,
	}
	if _, err := jobs.Enqueue(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func randomID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "ord_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return "ord_" + hex.EncodeToString(buffer)
}
