// Package queue is the durable work ledger every slow or unreliable step runs
// through: creating a charge at a payment provider, reading a payment back
// after a webhook, releasing a hold that lapsed. RabbitMQ is the fast delivery
// path; these rows make the work atomic with the business transaction.
//
// Durable, not in-process: a job survives a deploy, a crash and a provider
// outage, and is retried with backoff until it succeeds or is parked. An
// in-memory channel loses exactly the jobs that matter, the ones in flight
// when the process died holding a buyer's money.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"
)

// Job types. Each one names a handler registered on the worker.
const (
	// TypeCreateCharge asks the payment provider for a charge for an order.
	TypeCreateCharge = "charge.create"
	// TypeSyncPayment reads a payment back from the provider and reconciles the
	// order with it. Enqueued by the webhook and by the reconciliation sweep.
	TypeSyncPayment = "payment.sync"
	// TypeExpireHolds releases stock from orders whose hold window has passed.
	TypeExpireHolds = "hold.expire"
	// TypeReconcile re-reads payments for orders that are still pending, which
	// is what makes the system correct even if every webhook is lost.
	TypeReconcile = "payment.reconcile"
	// TypeAuditSettled re-reads payments for orders that are already PAID, on a
	// much slower cadence.
	//
	// Reconciliation only ever looks at orders still waiting for money, so a
	// refund or a chargeback issued in the provider's own dashboard reaches the
	// box office through exactly one notification. Lose it and the order stays
	// paid and the seat stays sold forever: money returned, inventory not.
	TypeAuditSettled = "payment.audit"
	// TypeRefundCharge gives money back through the same durable ledger as
	// every other provider call, so a refund survives a slow provider and a
	// restart instead of dying with the request that asked for it.
	TypeRefundCharge = "charge.refund"
	// TypeCancelCharge voids the provider charge of an order whose hold lapsed,
	// so the PIX code stops being payable the moment the seat goes back on
	// sale.
	//
	// It exists because the two clocks do not agree: a hold is thirty minutes
	// and a provider dates a charge to a DAY, so releasing the stock used to
	// leave a live, payable code out in the world for another twenty-odd hours.
	// Every one of those that got paid became money taken for a seat somebody
	// else had already bought. Cancelling closes the window to the hold itself.
	TypeCancelCharge = "charge.cancel"
	// TypeCleanup removes old completed work and expired idempotency keys in
	// bounded batches. Dead jobs and business records are never removed here.
	TypeCleanup = "maintenance.cleanup"
	// TypeSendNotification delivers one message to one buyer over one channel.
	// It is here, in the same ledger as the money, because telling somebody
	// their ingressos are theirs is not best-effort work: the job row is
	// written in the transaction that settled the payment, so the receipt
	// cannot exist without the payment and cannot be lost with a process.
	TypeSendNotification = "notification.send"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusDone       Status = "done"
	// StatusDead is a job that exhausted its attempts. It is kept, not deleted:
	// a parked job is the record of money or stock that needs a human.
	StatusDead Status = "dead"
)

var (
	ErrNotFound = errors.New("job not found")
	// ErrRetry lets a handler ask for another attempt without describing a
	// failure, for the ordinary "not ready yet" case.
	ErrRetry = errors.New("job should be retried")
)

// PermanentError marks a failure no retry can fix: the order the job refers to
// does not exist, the payload cannot be decoded, the provider rejected the
// request outright. The worker parks such a job on the first attempt instead
// of spending the whole retry budget discovering the same thing eight times.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return "permanent: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err so the worker will not retry it.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err was marked as not worth retrying.
func IsPermanent(err error) bool {
	var permanent *PermanentError
	return errors.As(err, &permanent)
}

// Job is one unit of durable work.
type Job struct {
	ID      string
	Type    string
	Payload json.RawMessage

	Status      Status
	Attempts    int
	MaxAttempts int
	// RunAt is when the job becomes claimable. Backoff is expressed by pushing
	// it into the future rather than by sleeping in a worker.
	RunAt     time.Time
	LastError string
	// DedupeKey collapses duplicates: enqueueing the same key twice while the
	// first is still open is a no-op. It is what stops a provider that
	// redelivers a webhook five times from creating five sync jobs.
	DedupeKey string
	// Rerun is set on a RUNNING job when a duplicate arrives for its key. The
	// running attempt may have read the provider a moment before the event the
	// duplicate announces, so completing it re-queues it for one more read
	// instead of finishing it and losing that event.
	Rerun bool
	// LockedBy and LockedAt identify the worker holding the job, so a crashed
	// worker's jobs can be reclaimed.
	LockedBy  string
	LockedAt  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DefaultMaxAttempts is generous on purpose: the failures this queue sees are
// mostly a payment provider being briefly unavailable, and giving up early
// leaves an order stuck that would have settled on the next try.
const DefaultMaxAttempts = 8

// Backoff is the delay before attempt n+1, exponential with a ceiling.
//
// Capped because these jobs are user-visible: a buyer is watching a screen for
// a PIX code, and an hour-long backoff is indistinguishable from broken.
func Backoff(attempts int) time.Duration {
	const base = 5 * time.Second
	const ceiling = 5 * time.Minute
	delay := base
	for i := 1; i < attempts && delay < ceiling; i++ {
		delay *= 2
	}
	if delay > ceiling {
		delay = ceiling
	}
	return delay
}

// Paced is an error that knows when it may be tried again.
//
// A rate limiter answers 429 with a Retry-After, and that number is the only
// one that matters: it is the provider saying exactly how long it will keep
// refusing. Guessing shorter gets refused again and, on most providers,
// extends the penalty; guessing longer leaves a buyer watching a checkout with
// no PIX code for minutes. Adapters attach it; nothing in the domain knows what
// an HTTP header is.
type Paced interface {
	// RetryAfter is how long to wait, or zero when the provider did not say.
	RetryAfter() time.Duration
}

// RetryAfter reads a provider's own pacing out of an error, if it gave one.
func RetryAfter(err error) (time.Duration, bool) {
	var paced Paced
	if errors.As(err, &paced) {
		if delay := paced.RetryAfter(); delay > 0 {
			return delay, true
		}
	}
	return 0, false
}

// RetryDelay is how long to wait before attempt n+1, all things considered.
//
// The provider's own instruction wins over the curve, because it is fact
// rather than estimate. Everything is then jittered, which is the part that
// matters under load: without it, a hundred jobs refused in the same second
// come back in the same second, and the herd that tripped the limit trips it
// again in lockstep. Providers ask for jitter for exactly this reason.
func RetryDelay(err error, attempts int) time.Duration {
	if delay, ok := RetryAfter(err); ok {
		return jitter(delay)
	}
	return jitter(Backoff(attempts))
}

// jitter spreads a delay, and only ever FORWARD.
//
// Additive rather than the more common "wait between half and all of it",
// because both inputs here are floors and not targets: Retry-After is the
// provider telling us it will refuse until then, and returning early is the
// one thing that reliably makes a rate limit worse. The spread is capped so a
// five-minute backoff cannot quietly become eight.
func jitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return delay
	}
	spread := delay / 2
	if spread > 30*time.Second {
		spread = 30 * time.Second
	}
	if spread <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int64N(int64(spread)+1))
}

// Queue is the persistence port for jobs.
type Queue interface {
	// Enqueue adds a job and reports whether it did. False with a nil error
	// means an open job with the same DedupeKey already exists: one still
	// waiting absorbs the duplicate, one already running is flagged to run once
	// more when it finishes. Either way the work is scheduled, and either way
	// the database decided it without an error; a duplicate is expected
	// traffic, not a failed statement.
	Enqueue(ctx context.Context, job *Job) (added bool, err error)
	// Claim locks up to `limit` due jobs for one worker. Implementations must
	// skip rows already locked by another worker rather than waiting on them.
	Claim(ctx context.Context, workerID string, types []string, limit int, now time.Time) ([]Job, error)
	// ClaimByID locks one named job, reporting false when it is not claimable:
	// already running, already done, or not yet due.
	//
	// This is what makes broker delivery exactly-once in effect. A redelivered
	// message names a job that has already been claimed, the claim fails, and
	// the duplicate does nothing instead of charging a buyer twice.
	ClaimByID(ctx context.Context, workerID, jobID string, now time.Time) (*Job, bool, error)
	// Complete marks a claimed job done, or, when a duplicate was enqueued
	// while it ran, returns it to the queue for one more run.
	Complete(ctx context.Context, jobID string, now time.Time) error
	// Retry returns a job to the queue with a later RunAt.
	Retry(ctx context.Context, jobID string, reason string, runAt time.Time, now time.Time) error
	// Kill parks a job that exhausted its attempts.
	Kill(ctx context.Context, jobID string, reason string, now time.Time) error
	// ReclaimStale returns jobs whose worker died back to the queue.
	ReclaimStale(ctx context.Context, olderThan time.Time, now time.Time) (int, error)
	// CountByStatus powers the health endpoint and the tests.
	CountByStatus(ctx context.Context, status Status) (int64, error)
}

// CompletedPruner is the retention capability implemented by persistent
// queues. It is separate from Queue so alternate transports and small test
// doubles do not need a destructive maintenance operation.
type CompletedPruner interface {
	DeleteCompletedBefore(ctx context.Context, olderThan time.Time, limit int) (int64, error)
}

// Payloads. Small and explicit: a job carries ids, never whole entities, so a
// job that runs an hour later reads current state rather than acting on a
// snapshot of the world as it was when it was enqueued.

type CreateChargePayload struct {
	OrderID string `json:"orderId"`
}

type SyncPaymentPayload struct {
	OrderID string `json:"orderId,omitempty"`
	// PaymentID is set when the trigger was a webhook, which names the payment
	// and not the order.
	PaymentID string `json:"paymentId,omitempty"`
	// EventID is the provider's delivery id, carried for logging and dedupe.
	EventID string `json:"eventId,omitempty"`
}

// NewPayload marshals a payload, returning nil on failure so a caller can
// decide; a job with an unmarshalable payload is a programming error.
func NewPayload(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}
