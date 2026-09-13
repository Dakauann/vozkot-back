// Package notification decides what a buyer is told and hands the telling to
// the durable queue.
//
// It is split the way the rest of this system is split. Notifier is the WRITE
// side: it turns a request into a job row inside somebody else's transaction,
// and returns without touching a provider. Service is the READ side: the
// worker calls it, it renders and it delivers. Purchases is the only place
// that knows an order has a buyer worth writing to.
//
// Nothing here retries, sleeps or counts attempts. That already exists once,
// correctly, in usecases/queue, and a second copy of it living in an email
// consumer is how two systems end up disagreeing about how many times
// something was sent.
package notification

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"time"

	domain "vozkot/domain/notification"
	"vozkot/domain/queue"
	queueUsecase "vozkot/usecases/queue"
)

// Notifier is the one way anything in this system asks for a message to be
// sent, and it never sends one: it writes a job row.
//
// That indirection is the entire point. A buyer's payment is settled inside a
// transaction that locks the order and moves stock; calling Resend from in
// there would hold a row lock open across a network round trip to a third
// party, and a provider having a slow minute would become a box office having
// a slow minute. Writing a row costs microseconds, commits with the payment,
// and cannot be lost afterwards.
//
// A nil *Notifier is a supported, meaningful value: it is what the container
// installs when no provider is configured. Every method is safe on it, so no
// call site is littered with "if notifications != nil".
type Notifier struct {
	jobs       queue.Queue
	dispatcher *queueUsecase.Dispatcher
	// channels is what actually has a Sender behind it. Enqueueing for a
	// channel nobody can deliver would write jobs that can only ever be
	// parked, which is a dead-letter queue filling up with work that was never
	// possible rather than work that failed.
	channels map[domain.Channel]struct{}
	now      func() time.Time
	newID    func() string
}

func NewNotifier(jobs queue.Queue, dispatcher *queueUsecase.Dispatcher, channels ...domain.Channel) *Notifier {
	available := make(map[domain.Channel]struct{}, len(channels))
	for _, channel := range channels {
		available[channel] = struct{}{}
	}
	return &Notifier{
		jobs:       jobs,
		dispatcher: dispatcher,
		channels:   available,
		now:        time.Now,
		newID:      randomID,
	}
}

// Handles reports whether a channel can be delivered on at all.
func (n *Notifier) Handles(channel domain.Channel) bool {
	if n == nil {
		return false
	}
	_, ok := n.channels[channel]
	return ok
}

// Enqueue writes the notification's job row through the queue handle it is
// given, which is how a caller puts the message inside its own transaction:
// pass repositories.Jobs() from a unit of work and the receipt commits with
// the payment it confirms, or neither does.
//
// It returns the job so the caller can announce it to the broker AFTER the
// commit — never inside it, for the same reason checkout does not. A nil job
// with a nil error means there was nothing to write: notifications are off, an
// identical message is already queued, or the request cannot be delivered on
// any registered channel.
func (n *Notifier) Enqueue(ctx context.Context, jobs queue.Queue, request domain.Request) (*queue.Job, error) {
	if n == nil {
		return nil, nil
	}
	if jobs == nil {
		jobs = n.jobs
	}
	if jobs == nil {
		return nil, nil
	}
	if !n.Handles(request.Channel) {
		log.Printf("notifications: no sender for channel %q; %s to %s was not queued",
			request.Channel, request.Template, request.Recipient.Address(request.Channel))
		return nil, nil
	}
	if err := request.Validate(); err != nil {
		// A buyer without an address is not an error the caller can act on —
		// an order can legitimately carry one this channel cannot reach — so
		// it is logged and skipped rather than failing the payment that was
		// being settled around it.
		log.Printf("notifications: skipping %s on %s: %v", request.Template, request.Channel, err)
		return nil, nil
	}

	payload, err := queue.NewPayload(domain.NewPayload(request))
	if err != nil {
		return nil, err
	}
	job := &queue.Job{
		ID:          n.newID(),
		Type:        queue.TypeSendNotification,
		Payload:     payload,
		RunAt:       n.now(),
		MaxAttempts: domain.MaxDeliveryAttempts,
		DedupeKey:   request.DedupeKey,
	}
	added, err := jobs.Enqueue(ctx, job)
	if err != nil {
		return nil, err
	}
	if !added {
		// The same message is already queued. A webhook redelivered five times
		// is five settlements that each want to send one receipt, and exactly
		// one of them wins here — decided by the database, without an error.
		return nil, nil
	}
	return job, nil
}

// Notify enqueues and announces in one call, for a caller with no transaction
// of its own to join.
func (n *Notifier) Notify(ctx context.Context, request domain.Request) error {
	if n == nil {
		return nil
	}
	job, err := n.Enqueue(ctx, nil, request)
	if err != nil {
		return err
	}
	n.dispatcher.Dispatch(ctx, job)
	return nil
}

func randomID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "ntf_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return "ntf_" + hex.EncodeToString(buffer)
}
