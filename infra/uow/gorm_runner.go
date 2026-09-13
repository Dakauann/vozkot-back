// Package uow binds the domain's transaction port to GORM.
//
// Every repository in this system takes a *gorm.DB, and a transaction handle is
// one. That is the whole trick: the runner opens a transaction and builds a
// fresh set of repositories on the transaction handle, so a use case writing
// through them writes inside it without ever knowing it exists.
package uow

import (
	"context"

	"vozkot/domain/event"
	"vozkot/domain/order"
	"vozkot/domain/queue"
	"vozkot/domain/ticket"
	domain "vozkot/domain/uow"
	eventRepository "vozkot/infra/repositories/event"
	orderRepository "vozkot/infra/repositories/order"
	queueRepository "vozkot/infra/repositories/queue"
	ticketRepository "vozkot/infra/repositories/ticket"

	"gorm.io/gorm"
)

type Runner struct {
	db *gorm.DB
}

func NewRunner(db *gorm.DB) *Runner { return &Runner{db: db} }

var _ domain.Runner = (*Runner)(nil)

// Run executes fn in a transaction: committed when it returns nil, rolled back
// on any error, and rolled back on a panic before the panic continues.
func (r *Runner) Run(ctx context.Context, fn func(ctx context.Context, repositories domain.Repositories) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(ctx, &repositories{tx: tx})
	})
}

type repositories struct {
	tx *gorm.DB
}

func (r *repositories) Orders() order.Repository { return orderRepository.NewOrderRepository(r.tx) }
func (r *repositories) Tickets() ticket.Repository {
	return ticketRepository.NewTicketRepository(r.tx)
}
func (r *repositories) Events() event.Repository { return eventRepository.NewEventRepository(r.tx) }
func (r *repositories) Jobs() queue.Queue        { return queueRepository.NewJobRepository(r.tx) }
