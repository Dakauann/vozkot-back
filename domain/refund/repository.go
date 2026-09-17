package refund

import (
	"context"
	"time"
)

// Filter selects refund requests for a listing.
type Filter struct {
	// EventID is the organiser's question: what is waiting on me for this show.
	EventID string
	// BuyerID scopes to one account's own requests.
	BuyerID string
	OrderID string
	Status  Status
	// Open selects pending and approved together, which is what an organiser's
	// inbox badge counts and what the "already has one in flight" check asks.
	Open   bool
	Limit  int
	Offset int
}

// Page is a listing plus the total it was taken from.
type Page struct {
	Items []Request
	Total int64
}

// Repository persists refund requests.
type Repository interface {
	// Create writes a request and refuses a second open one on the same order.
	//
	// It returns ErrAlreadyOpen when one exists, decided by a partial unique
	// index rather than by a prior SELECT: a buyer double-tapping "cancelar"
	// is exactly the race a read-then-write loses, and losing it means two
	// refunds for one order.
	Create(ctx context.Context, item *Request) error
	GetByID(ctx context.Context, id string) (*Request, error)
	// GetByIDForUpdate holds the row until the transaction ends, so two people
	// deciding the same request take turns and the second sees the first's
	// answer instead of overwriting it.
	GetByIDForUpdate(ctx context.Context, id string) (*Request, error)
	// FindOpenByOrder returns the request currently occupying an order, if any.
	FindOpenByOrder(ctx context.Context, orderID string) (*Request, error)
	List(ctx context.Context, filter Filter) (Page, error)
	Update(ctx context.Context, item *Request) error
	// CountOpenByOrder reports which of a set of orders already have a request
	// in flight, in ONE query.
	//
	// It exists for the orders listing: a page of twenty orders each asking
	// "am I refundable" one at a time is the N+1 that makes a buyer's order
	// history slow, and the answer is a single grouped read.
	OpenByOrders(ctx context.Context, orderIDs []string) (map[string]Request, error)
}

// EventTimingFor is how a use case learns when a show is without this package
// importing the catalogue.
//
// A function type rather than an interface because there is exactly one method
// and the caller already has the event repository; it keeps domain/refund
// depending on nothing but the standard library.
type EventTimingFor func(ctx context.Context, eventID string) (EventTiming, error)

// Clock is the time source a use case passes in, named so a test reads clearly.
type Clock func() time.Time
