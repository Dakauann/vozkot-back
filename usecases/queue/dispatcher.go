package queue

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	domain "vozkot/domain/queue"
)

// Dispatcher announces jobs to the broker AFTER the transaction that wrote them
// has committed.
//
// The ordering is the whole point. Publishing inside the transaction would
// announce work for an order that might still roll back — and a consumer is
// fast enough to look for that order before it exists. Publishing after the
// commit can only lose the announcement, never invent one, and a lost
// announcement is exactly what the poller is there for.
type Dispatcher struct {
	publisher         domain.Publisher
	now               func() time.Time
	inFlight          chan struct{}
	lastSaturationLog atomic.Int64
}

const maxDispatchInFlight = 256

func NewDispatcher(publisher domain.Publisher) *Dispatcher {
	return &Dispatcher{publisher: publisher, now: time.Now, inFlight: make(chan struct{}, maxDispatchInFlight)}
}

// Dispatch publishes the jobs that are due now.
//
// A job scheduled for the future — a hold expiring in thirty minutes — is not
// published: the broker has no notion of "deliver this later" without a plugin,
// and the poller already wakes for exactly that. Publishing it now would only
// produce a message that claims nothing and is thrown away.
func (d *Dispatcher) Dispatch(ctx context.Context, jobs ...*domain.Job) {
	if d == nil || d.publisher == nil {
		return
	}
	now := d.now()
	for _, job := range jobs {
		if job == nil || job.ID == "" {
			continue
		}
		if job.RunAt.After(now) {
			continue
		}
		select {
		case d.inFlight <- struct{}{}:
			message := domain.Message{JobID: job.ID, Type: job.Type}
			go func() {
				defer func() { <-d.inFlight }()
				// The HTTP request is allowed to finish as soon as its durable row
				// commits. Publishing is an accelerator with its own short bound.
				publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if err := d.publisher.Publish(publishCtx, message); err != nil {
					// Not fatal, and deliberately not retried here: the row is
					// committed, so the poller will pick the job up within one
					// interval. The log tells an operator the broker is sick.
					log.Printf("queue: publish %s (%s): %v", message.JobID, message.Type, err)
				}
			}()
		default:
			// Never turn a synchronized on-sale into one goroutine per buyer.
			// The job row is already durable; a full accelerator simply means
			// this job is delivered by the database poller.
			second := time.Now().Unix()
			previous := d.lastSaturationLog.Load()
			if previous != second && d.lastSaturationLog.CompareAndSwap(previous, second) {
				log.Printf("queue: broker dispatch saturated; overflow jobs will be picked up by the poller")
			}
		}
	}
}
