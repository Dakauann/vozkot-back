package ticket

import "context"

// Sort names the orders the list endpoint offers. A box office reads its
// catalogue by door date far more often than by when a row was typed, so that
// is the default.
type Sort string

const (
	SortStartsAt  Sort = "starts_at"
	SortCreatedAt Sort = "created_at"
	SortPrice     Sort = "price"
)

// Filter is the query a listing is built from. The zero value lists everything.
type Filter struct {
	Status  Status
	Query   string
	OwnerID string
	Sort    Sort
	Limit   int
	Offset  int
}

// Repository is the persistence port. Only infra implements it, and only
// use cases call it.
type Repository interface {
	Create(ctx context.Context, item *Ticket) error
	GetByID(ctx context.Context, id string) (*Ticket, error)
	List(ctx context.Context, filter Filter) ([]Ticket, error)
	Count(ctx context.Context, filter Filter) (int64, error)
	Update(ctx context.Context, item *Ticket) error
	Delete(ctx context.Context, id string) error

	// Reserve takes `quantity` tickets out of availability and reports whether
	// it succeeded.
	//
	// This is the one operation that MUST be atomic at the database, not in Go:
	// read-then-write here is the textbook oversell, where two requests both
	// read "3 left" and both reserve 2. Implementations perform a single
	// conditional update and report false when it matched no row.
	Reserve(ctx context.Context, ticketID string, quantity int) (bool, error)
	// Release returns held stock to availability, for a hold that expired or an
	// order that was cancelled.
	Release(ctx context.Context, ticketID string, quantity int) error
	// Commit converts a hold into a sale. It is the only path to `sold`.
	Commit(ctx context.Context, ticketID string, quantity int) error
	// ReleaseSold gives a paid ticket back, for a refund or a chargeback.
	ReleaseSold(ctx context.Context, ticketID string, quantity int) error
}
