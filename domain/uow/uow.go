// Package uow is the transaction boundary for writes that span more than one
// aggregate.
//
// Checkout touches inventory and an order; settling a payment touches an order
// and inventory again. Neither pair may half-happen: an order marked paid whose
// stock was never committed oversells the next buyer, and stock committed
// against an order that stayed pending sells a ticket twice. A use case
// therefore asks for a unit of work and does both writes inside it.
//
// The port lives in the domain so use cases never import a database package;
// infra supplies the implementation, and a test supplies one that runs the
// function against in-memory repositories.
package uow

import (
	"context"

	"vozkot/domain/admission"
	"vozkot/domain/event"
	"vozkot/domain/order"
	"vozkot/domain/queue"
	"vozkot/domain/refund"
	"vozkot/domain/seating"
	"vozkot/domain/ticket"
)

// Repositories is the set of ports bound to one transaction.
type Repositories interface {
	Orders() order.Repository
	Tickets() ticket.Repository
	// Events is read during settlement: a receipt names the show, the door
	// time and the venue, and those live on the event rather than on the tier
	// whose price was paid.
	Events() event.Repository
	// Refunds is written in the same transaction that enqueues the money
	// movement behind it. A refund request approved in one transaction and a
	// refund job enqueued in another is a pair that can half-happen: an
	// approval nobody acts on, or money leaving with no record of who allowed
	// it. Both are worse than the extra binding here.
	Refunds() refund.Repository
	// Admissions is written in the transaction that marks an order paid.
	//
	// A paid order with no tickets issued is a buyer holding a receipt and no
	// way in, and a set of tickets issued against a payment that then rolled
	// back is entry somebody never paid for. The two have to commit together,
	// which is the same argument the stock above rests on.
	Admissions() admission.Repository
	// Seats is reserved seating, and it is bound here for the same reason
	// Tickets is: a seat held against an order that was never written is
	// inventory nothing will release, and an order written against a seat
	// somebody else got is two tickets for one chair.
	//
	// A general-admission event never reaches it. The tier counters are still
	// the whole story for a party, and this port is only touched by a line that
	// names seats.
	Seats() seating.Repository
	Jobs() queue.Queue
}

// Runner executes fn inside a transaction, committing when it returns nil and
// rolling back on any error or panic.
type Runner interface {
	Run(ctx context.Context, fn func(ctx context.Context, repositories Repositories) error) error
}
