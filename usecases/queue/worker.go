// Package queue runs the durable jobs: it claims work, dispatches it, and
// decides what happens when it fails.
//
// Work reaches a worker two ways, and both end at the same place:
//
//   - The BROKER delivers a message naming a job, milliseconds after the
//     transaction that wrote it committed. This is the path a buyer feels.
//   - The POLLER sweeps the table for jobs that are due, which covers everything
//     the broker cannot: scheduled work (a hold expiring in thirty minutes), a
//     retry waiting out its backoff, and any publish lost while the broker was
//     down.
//
// Either way the job ROW is claimed before a handler runs, and that claim is
// what makes the whole thing exactly-once in effect: a redelivered message
// claims nothing and does nothing.
//
// Jobs run concurrently on both paths. The work is I/O, a round trip to a
// payment provider, so one job at a time per worker would leave a process
// idle for hundreds of milliseconds per job. The broker's prefetch bounds the
// broker path; the batch size bounds the poller's. Size the database pool to
// cover both (docs/SCALE.md).
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	metricsdomain "vozkot/domain/metrics"
	domain "vozkot/domain/queue"
)

// Handler processes one job. Returning nil completes it; returning an error
// schedules a retry until the attempts run out.
type Handler func(ctx context.Context, job domain.Job) error

type Worker struct {
	jobs     domain.Queue
	broker   domain.Consumer
	handlers map[string]Handler
	id       string
	batch    int
	interval time.Duration
	// staleAfter is how long a claimed job may stay locked before it is assumed
	// its worker died. It must exceed the slowest handler, or a long payment
	// call would be reclaimed and run twice.
	staleAfter time.Duration
	// jobTimeout bounds one attempt. It is what makes staleAfter honest: a
	// handler that cannot exceed it can never still be running when the sweep
	// decides its worker is dead.
	jobTimeout time.Duration
	// metrics counts jobs that were parked. Optional: nil records nothing and
	// the worker behaves identically.
	metrics metricsdomain.QueueRecorder
	now     func() time.Time
}

type Option func(*Worker)

// WithMetrics reports parked jobs.
//
// A parked job is money or stock that needs a person, and until this existed
// the only trace was a log line. Optional, because a deployment with no
// monitoring must run exactly as it did before.
func WithMetrics(recorder metricsdomain.QueueRecorder) Option {
	return func(w *Worker) { w.metrics = recorder }
}

func WithBatchSize(size int) Option {
	return func(w *Worker) {
		if size > 0 {
			w.batch = size
		}
	}
}

func WithInterval(interval time.Duration) Option {
	return func(w *Worker) {
		if interval > 0 {
			w.interval = interval
		}
	}
}

// WithJobTimeout bounds a single attempt. Keep it well under the stale
// window, which NewWorker enforces.
func WithJobTimeout(timeout time.Duration) Option {
	return func(w *Worker) {
		if timeout > 0 {
			w.jobTimeout = timeout
		}
	}
}

func WithClock(now func() time.Time) Option {
	return func(w *Worker) {
		if now != nil {
			w.now = now
		}
	}
}

// WithBroker attaches the message transport. Without one the worker still
// works, on the poller alone.
func WithBroker(consumer domain.Consumer) Option {
	return func(w *Worker) { w.broker = consumer }
}

func NewWorker(jobs domain.Queue, opts ...Option) *Worker {
	hostname, _ := os.Hostname()
	worker := &Worker{
		jobs:       jobs,
		handlers:   map[string]Handler{},
		id:         fmt.Sprintf("%s-%s", hostname, randomID()),
		batch:      10,
		interval:   time.Second,
		staleAfter: 5 * time.Minute,
		jobTimeout: 2 * time.Minute,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(worker)
	}
	// The sweep must never reclaim a job that is still running: two workers on
	// one job is the one thing claiming exists to prevent.
	if worker.staleAfter <= worker.jobTimeout {
		worker.staleAfter = 2 * worker.jobTimeout
	}
	return worker
}

// Handle registers the handler for a job type. A job whose type has no handler
// is parked rather than retried, because no number of retries will grow one.
func (w *Worker) Handle(jobType string, handler Handler) {
	w.handlers[jobType] = handler
}

// Types lists what this worker will claim.
func (w *Worker) Types() []string {
	types := make([]string, 0, len(w.handlers))
	for jobType := range w.handlers {
		types = append(types, jobType)
	}
	return types
}

// ID identifies this worker in the lock column.
func (w *Worker) ID() string { return w.id }

// Run consumes from the broker and polls the table until the context ends.
func (w *Worker) Run(ctx context.Context) {
	if w.broker != nil {
		go w.consume(ctx)
	}

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	reclaim := time.NewTicker(w.staleAfter)
	defer reclaim.Stop()

	log.Printf("queue: worker %s started for %v", w.id, w.Types())
	for {
		select {
		case <-ctx.Done():
			log.Printf("queue: worker %s stopped", w.id)
			return
		case <-reclaim.C:
			cutoff := w.now().Add(-w.staleAfter)
			if count, err := w.jobs.ReclaimStale(ctx, cutoff, w.now()); err != nil {
				log.Printf("queue: reclaim stale jobs: %v", err)
			} else if count > 0 {
				log.Printf("queue: reclaimed %d stale job(s)", count)
			}
		case <-ticker.C:
			// Keep draining while there is a full batch: a burst should not be
			// spread across one job per tick.
			for {
				processed, err := w.ProcessBatch(ctx)
				if err != nil {
					log.Printf("queue: claim jobs: %v", err)
					break
				}
				if processed < w.batch {
					break
				}
			}
		}
	}
}

// consume runs the broker loop, reconnecting when it drops.
func (w *Worker) consume(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := w.broker.Consume(ctx, func(ctx context.Context, message domain.Message) error {
			return w.ProcessMessage(ctx, message)
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("queue: broker consumption stopped (%v); the poller keeps running", err)
		}
		// The poller covers this gap, so a broker outage slows delivery instead
		// of stopping it.
		time.Sleep(2 * time.Second)
	}
}

// ProcessMessage runs the job a broker delivery names.
//
// A message for a job that is already done, already running, or not yet due
// claims nothing and returns: that is the deduplication, and it is why the
// broker can deliver at-least-once without the side effects happening twice.
func (w *Worker) ProcessMessage(ctx context.Context, message domain.Message) error {
	job, claimed, err := w.jobs.ClaimByID(ctx, w.id, message.JobID, w.now())
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	w.run(ctx, *job)
	return nil
}

// ProcessBatch claims up to one batch of due jobs, runs them all at once, and
// returns how many there were once every one has been completed, retried or
// parked. The batch size is the poller's in-flight bound, as prefetch is the
// broker's.
func (w *Worker) ProcessBatch(ctx context.Context) (int, error) {
	claimed, err := w.jobs.Claim(ctx, w.id, w.Types(), w.batch, w.now())
	if err != nil {
		return 0, err
	}
	var inFlight sync.WaitGroup
	for index := range claimed {
		inFlight.Add(1)
		go func(job domain.Job) {
			defer inFlight.Done()
			w.run(ctx, job)
		}(claimed[index])
	}
	inFlight.Wait()
	return len(claimed), nil
}

// run executes one claimed job and records what happened.
//
// The job is deliberately detached from the caller's context and given a
// deadline of its own. Once a row is claimed, the work is this worker's
// responsibility: cancelling it because a deploy started would abandon a
// buyer's charge halfway AND abandon the bookkeeping that says it was done,
// leaving a finished job marked processing until the stale sweep ran it a
// second time. So a shutdown stops the worker from claiming ANYTHING NEW;
// the claim calls still use the cancellable context, while whatever is
// already in hand runs to its end, which is what both ProcessBatch and the
// broker consumer wait for before returning.
func (w *Worker) run(ctx context.Context, job domain.Job) {
	jobCtx, release := context.WithTimeout(context.WithoutCancel(ctx), w.jobTimeout)

	handler, ok := w.handlers[job.Type]
	if !ok {
		release()
		finalizeCtx, finalize := finalizationContext(ctx)
		defer finalize()
		w.kill(finalizeCtx, job, "no handler registered for type "+job.Type)
		return
	}

	err := safely(jobCtx, handler, job)
	release()

	// Execution and bookkeeping deliberately have separate deadlines. A
	// handler that uses its entire attempt deadline returns with jobCtx already
	// cancelled; using that context for Complete/Retry/Kill leaves the row in
	// processing until the stale-worker sweep. At payment scale that is both a
	// long customer delay and an avoidable duplicate-attempt risk.
	finalizeCtx, finalize := finalizationContext(ctx)
	defer finalize()
	if err == nil {
		if completeErr := w.jobs.Complete(finalizeCtx, job.ID, w.now()); completeErr != nil {
			log.Printf("queue: complete job %s: %v", job.ID, completeErr)
		}
		return
	}

	// A permanent failure is parked on the spot: retrying "order not found"
	// eight times over ten minutes tells nobody anything the first attempt
	// did not, and keeps a job that needs a human hidden in the retry queue.
	if domain.IsPermanent(err) {
		w.kill(finalizeCtx, job, err.Error())
		return
	}
	// Attempts already includes this one: claiming increments it.
	if job.Attempts >= job.MaxAttempts {
		w.kill(finalizeCtx, job, err.Error())
		return
	}
	runAt := w.now().Add(domain.Backoff(job.Attempts))
	if retryErr := w.jobs.Retry(finalizeCtx, job.ID, err.Error(), runAt, w.now()); retryErr != nil {
		log.Printf("queue: retry job %s: %v", job.ID, retryErr)
		return
	}
	log.Printf("queue: job %s (%s) failed on attempt %d/%d, retrying in %s: %v",
		job.ID, job.Type, job.Attempts, job.MaxAttempts, domain.Backoff(job.Attempts), err)
}

const finalizationTimeout = 10 * time.Second

// finalizationContext preserves request-scoped values for tracing but not the
// caller's cancellation or the handler deadline. Recording the attempt is a
// separate, short database operation that must still run during shutdown.
func finalizationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), finalizationTimeout)
}

func (w *Worker) kill(ctx context.Context, job domain.Job, reason string) {
	if err := w.jobs.Kill(ctx, job.ID, reason, w.now()); err != nil {
		log.Printf("queue: park job %s: %v", job.ID, err)
		return
	}
	// A parked job is money or stock that needs a person, so it is logged at
	// the loudest level this system has.
	log.Printf("queue: PARKED job %s (%s) after %d attempt(s): %s", job.ID, job.Type, job.Attempts, reason)
	// Counted as well as logged, and counted AFTER the row is written so the
	// number can never claim a parking that did not commit.
	if w.metrics != nil {
		w.metrics.IncJobParked(job.Type)
	}
}

// safely turns a panicking handler into a failed job instead of a dead worker.
func safely(ctx context.Context, handler Handler, job domain.Job) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("handler panicked: %v", recovered)
		}
	}()
	return handler(ctx, job)
}

// Decode unmarshals a job payload, giving handlers one line instead of five.
// A payload that cannot be decoded never will be, so the error is permanent.
func Decode[T any](job domain.Job) (T, error) {
	var payload T
	if len(job.Payload) == 0 {
		return payload, domain.Permanent(errors.New("job payload is empty"))
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return payload, domain.Permanent(fmt.Errorf("decode %s payload: %w", job.Type, err))
	}
	return payload, nil
}

func randomID() string {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return time.Now().UTC().Format("150405.000000")
	}
	return hex.EncodeToString(buffer)
}
