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

	"vozkot/domain/order"
	"vozkot/domain/queue"
	"vozkot/domain/ticket"
)

// Repositories is the set of ports bound to one transaction.
type Repositories interface {
	Orders() order.Repository
	Tickets() ticket.Repository
	Jobs() queue.Queue
}

// Runner executes fn inside a transaction, committing when it returns nil and
// rolling back on any error or panic.
type Runner interface {
	Run(ctx context.Context, fn func(ctx context.Context, repositories Repositories) error) error
}
