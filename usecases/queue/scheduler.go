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
// lost; a webhook can never arrive. The sweeps are the floor under both — they
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

func (s *Scheduler) tick(ctx context.Context) {
	now := s.now()
	// The dedupe key carries the minute, so any number of application instances
	// ticking at once produce exactly one sweep job per minute.
	bucket := now.UTC().Truncate(time.Minute).Format("20060102T1504")

	for _, jobType := range []string{domain.TypeExpireHolds, domain.TypeReconcile, domain.TypeCleanup} {
		job := &domain.Job{
			ID:          "sch_" + s.newID(),
			Type:        jobType,
			Payload:     []byte(`{}`),
			RunAt:       now,
			MaxAttempts: 3,
			DedupeKey:   jobType + ":sweep:" + bucket,
		}
		added, err := s.jobs.Enqueue(ctx, job)
		if err != nil {
			log.Printf("queue: schedule %s: %v", jobType, err)
			continue
		}
		if !added {
			// Another instance ticked first this minute; its job is the one.
			continue
		}
		s.dispatcher.Dispatch(ctx, job)
	}
}
