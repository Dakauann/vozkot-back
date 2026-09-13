package order

import (
	"context"
	"time"
)

// Filter selects orders for a listing.
type Filter struct {
	TicketID string
	BuyerID  string
	Status   Status
	Limit    int
	Offset   int
	// OldestUpdatedFirst is used by recovery sweeps. Successful reconciliation
	// refreshes UpdatedAt, rotating still-pending orders behind work that has
	// not been checked yet instead of starving the oldest payment forever.
	OldestUpdatedFirst bool
	// EventNotBefore keeps a sweep to orders whose event has not happened yet.
	//
	// The settled-order audit needs it: re-reading every order ever paid, at
	// the provider, forever, is unbounded work for no benefit — once the doors
	// have closed a late refund is bookkeeping, not inventory.
	EventNotBefore time.Time
}

// Repository persists orders.
//
// FindByIdempotencyKey is what makes a retried checkout return the order the
// first attempt created instead of reserving a second batch of tickets, and
// ClaimExpired is what lets one sweeper release lapsed holds without two
// workers releasing the same stock twice.
type Repository interface {
	Create(ctx context.Context, item *Order) error
	GetByID(ctx context.Context, id string) (*Order, error)
	// GetByIDForUpdate reads the order and holds its row until the enclosing
	// transaction ends. Every read-then-write on an order goes through it, so
	// two settlements of the same order — a redelivered webhook racing the
	// reconciliation sweep — run one after the other, and the second sees
	// what the first committed instead of both seeing "pending".
	GetByIDForUpdate(ctx context.Context, id string) (*Order, error)
	FindByIdempotencyKey(ctx context.Context, key string) (*Order, error)
	FindByPaymentID(ctx context.Context, provider, paymentID string) (*Order, error)
	List(ctx context.Context, filter Filter) ([]Order, error)
	Count(ctx context.Context, filter Filter) (int64, error)
	Update(ctx context.Context, item *Order) error

	// ClaimExpired atomically marks up to `limit` lapsed pending orders as
	// expired and returns them, so the caller can release exactly the stock it
	// just claimed. Two concurrent sweepers therefore split the work rather
	// than both releasing the same orders.
	ClaimExpired(ctx context.Context, now time.Time, limit int) ([]Order, error)

	// CountOpenHoldsForUpdate serialises one buyer's concurrent checkouts and
	// reports what that buyer is already holding.
	//
	// "ForUpdate" because it takes a lock, exactly like GetByIDForUpdate, and
	// for the same reason: a count that is not held against the insert it
	// guards is decoration. Two checkouts by one account arriving together
	// would otherwise both read "one open order", both pass a limit of two, and
	// both commit — the check defeated by the only traffic that would try.
	//
	// The lock is per BUYER, so it costs an honest buyer nothing: it is only
	// ever contended by someone checking out twice at once, which is either a
	// retry or an attack.
	//
	// It is meaningful ONLY inside a unit of work. Called on its own the lock is
	// taken and released by the same implicit transaction, and the number is
	// stale before the caller reads it.
	CountOpenHoldsForUpdate(ctx context.Context, buyerID, ticketID string) (OpenHolds, error)
}
