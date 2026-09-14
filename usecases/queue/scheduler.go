package queue

import (
	"context"
	"log"
	"time"

	domain "vozkot/domain/queue"
)

// Scheduler enqueues the recurring jobs: the hold sweep, payment
// reconciliation and bounded retention cleanup.
//
// These exist because the precise paths can fail. A per-order expiry job can be
// lost; a webhook can never arrive. The sweeps are the floor under both, they
// cost one query a minute and they are the difference between "usually
// correct" and "correct".
type Scheduler struct {
	jobs       domain.Queue
	dispatcher *Dispatcher
	interval   time.Duration
	now        func() time.Time
	newID      func() string
}

func NewScheduler(jobs domain.Queue, dispatcher *Dispatcher, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Scheduler{jobs: jobs, dispatcher: dispatcher, interval: interval, now: time.Now, newID: randomID}
}

func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// Tick enqueues one round. Exported so a test can run it without waiting.
func (s *Scheduler) Tick(ctx context.Context) { s.tick(ctx) }

// sweeps are the recurring jobs and how often each one is due.
//
// The cadence is per sweep because the work is not comparable. Releasing a
// lapsed hold and recovering a lost notification are minutes-matter jobs: a
// buyer is waiting. Re-reading orders that are already settled is a safety net
// against a refund notification nobody received, and running that every minute
// would spend a provider call per paid order per minute to catch something that
// happens a few times a day.
var sweeps = []struct {
	jobType string
	every   time.Duration
}{
	{domain.TypeExpireHolds, time.Minute},
	{domain.TypeReconcile, time.Minute},
	{domain.TypeCleanup, time.Minute},
	{domain.TypeAuditSettled, time.Hour},
}

func (s *Scheduler) tick(ctx context.Context) {
	now := s.now()

	for _, sweep := range sweeps {
		// Each sweep is queued for the NEXT turn of its period rather than for
		// now, and that forward date is what makes the cadence real.
		//
		// The dedupe key carries the bucket, so any number of instances ticking
		// at once produce one job per period. But the unique index behind that
		// key only covers OPEN jobs, deliberately, a completed "sync order X"
		// must never block the next sync of order X. A sweep queued to run
		// immediately finishes in a second or two, leaves that index, and the
		// NEXT tick inserts the same bucket again with nothing to conflict
		// with. The hourly audit ran every minute for exactly that reason:
		// sixty provider calls per settled order per hour, to catch something
		// that happens a few times a day.
		//
		// Dated forward, the row stays pending for the whole period, holds the
		// key the entire time, and every tick in between is dropped. Nothing
		// else had to change for it: the dispatcher already declines to publish
		// a job that is not due, and the poller already claims on run_at.
		runAt := now.UTC().Truncate(sweep.every).Add(sweep.every)
		bucket := runAt.Format("20060102T150405")
		job := &domain.Job{
			ID:          "sch_" + s.newID(),
			Type:        sweep.jobType,
			Payload:     []byte(`{}`),
			RunAt:       runAt,
			MaxAttempts: 3,
			DedupeKey:   sweep.jobType + ":sweep:" + bucket,
		}
		added, err := s.jobs.Enqueue(ctx, job)
		if err != nil {
			log.Printf("queue: schedule %s: %v", sweep.jobType, err)
			continue
		}
		if !added {
			// Another instance ticked first this period; its job is the one.
			continue
		}
		s.dispatcher.Dispatch(ctx, job)
	}
}
