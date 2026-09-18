package order

import (
	"context"
	"time"
)

// Filter selects orders for a listing.
type Filter struct {
	// TicketID selects orders with a line for that tier, which is now a
	// question about the items rather than about the order.
	TicketID string
	// EventID selects every order for one night, across all of its tiers.
	EventID string
	BuyerID string
	Status  Status
	// Statuses selects several at once, for a caller asking a question that no
	// single status answers.
	//
	// The buyer's own list is the one that needs it: an abandoned checkout is
	// persisted here as an `expired` order, so a list of everything is mostly
	// carts nobody finished, and the orders that are actually alive -- paid,
	// still awaiting payment, owed a refund -- are buried among them. Three
	// separate requests could not be paged as one list.
	//
	// Applied together with Status, not instead of it, so a caller that sets
	// both gets the intersection rather than a silent winner.
	Statuses []Status
	Limit    int
	Offset   int
	// OldestUpdatedFirst is used by recovery sweeps. Successful reconciliation
	// refreshes UpdatedAt, rotating still-pending orders behind work that has
	// not been checked yet instead of starving the oldest payment forever.
	OldestUpdatedFirst bool
	// EventNotBefore keeps a sweep to orders whose event has not happened yet.
	//
	// The settled-order audit needs it: re-reading every order ever paid, at
	// the provider, forever, is unbounded work for no benefit, once the doors
	// have closed a late refund is bookkeeping, not inventory.
	EventNotBefore time.Time
	// UpdatedBefore keeps a sweep to orders nobody has looked at recently.
	//
	// It is what turns reconciliation from a poller into a safety net. Without
	// it the sweep re-reads EVERY pending order from the provider on every
	// pass, so one unpaid order costs a provider call a minute for the whole
	// length of its hold, and a busy onsale spends thousands of calls
	// discovering that nobody has paid yet. Settlement refreshes UpdatedAt even
	// when the charge has not moved, so each read pushes the next one out by
	// this much: the backoff is a property of the query rather than state
	// anybody has to keep.
	UpdatedBefore time.Time
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
	// two settlements of the same order, a redelivered webhook racing the
	// reconciliation sweep; run one after the other, and the second sees
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
	// both commit; the check defeated by the only traffic that would try.
	//
	// The lock is per BUYER, so it costs an honest buyer nothing: it is only
	// ever contended by someone checking out twice at once, which is either a
	// retry or an attack.
	//
	// It is meaningful ONLY inside a unit of work. Called on its own the lock is
	// taken and released by the same implicit transaction, and the number is
	// stale before the caller reads it.
	// ticketIDs are the tiers this checkout touches; the counts come back
	// only for those. Passing none still takes the lock and still counts open
	// orders, which is what a caller with no per-tier cap wants.
	CountOpenHoldsForUpdate(ctx context.Context, buyerID string, ticketIDs []string) (OpenHolds, error)
}
