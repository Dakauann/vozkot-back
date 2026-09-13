package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "vozkot/domain/queue"
	"vozkot/infra/rabbitmq"
	repository "vozkot/infra/repositories/queue"
	"vozkot/infra/testsupport"
)

// The queue under test is the PostgreSQL table, and the broker is RabbitMQ.
// Exclusive claiming, dedupe among open jobs and stale reclaim are all
// properties of SQL statements — a map with a mutex would only prove that a
// map with a mutex works.

// newQueue returns the real queue plus a job type unique to this test, so its
// rows are its own even while other packages use the same table.
func newQueue(t *testing.T) (*repository.JobRepository, string) {
	t.Helper()
	db := testsupport.Database(t)
	jobType := testsupport.Unique("test")
	testsupport.CleanupJobs(t, db, jobType)
	return repository.NewJobRepository(db), jobType
}

func enqueue(t *testing.T, jobs domain.Queue, job *domain.Job) *domain.Job {
	t.Helper()
	if job.ID == "" {
		job.ID = testsupport.Unique("job")
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 3
	}
	if job.Payload == nil {
		job.Payload = []byte(`{}`)
	}
	if job.RunAt.IsZero() {
		job.RunAt = time.Now()
	}
	if _, err := jobs.Enqueue(context.Background(), job); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	return job
}

// tryEnqueue is enqueue for tests that care whether the job was added.
func tryEnqueue(t *testing.T, jobs domain.Queue, job *domain.Job) bool {
	t.Helper()
	if job.ID == "" {
		job.ID = testsupport.Unique("job")
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 3
	}
	if job.Payload == nil {
		job.Payload = []byte(`{}`)
	}
	if job.RunAt.IsZero() {
		job.RunAt = time.Now()
	}
	added, err := jobs.Enqueue(context.Background(), job)
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	return added
}

func count(t *testing.T, jobType string, status domain.Status) int64 {
	t.Helper()
	return testsupport.CountJobs(t, testsupport.Database(t), jobType, string(status))
}

func TestWorkerCompletesASuccessfulJob(t *testing.T) {
	jobs, jobType := newQueue(t)
	worker := NewWorker(jobs)
	var ran atomic.Int32
	worker.Handle(jobType, func(context.Context, domain.Job) error {
		ran.Add(1)
		return nil
	})
	enqueue(t, jobs, &domain.Job{Type: jobType})

	processed, err := worker.ProcessBatch(context.Background())

	if err != nil || processed != 1 {
		t.Fatalf("ProcessBatch() = (%d, %v)", processed, err)
	}
	if ran.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", ran.Load())
	}
	if done := count(t, jobType, domain.StatusDone); done != 1 {
		t.Fatalf("done jobs = %d, want 1", done)
	}
}

func TestWorkerRetriesWithBackoffThenParks(t *testing.T) {
	jobs, jobType := newQueue(t)
	now := time.Now()
	worker := NewWorker(jobs, WithClock(func() time.Time { return now }))
	var attempts atomic.Int32
	worker.Handle(jobType, func(context.Context, domain.Job) error {
		attempts.Add(1)
		return errors.New("provider unavailable")
	})
	enqueue(t, jobs, &domain.Job{Type: jobType, RunAt: now, MaxAttempts: 3})

	if _, err := worker.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if pending := count(t, jobType, domain.StatusPending); pending != 1 {
		t.Fatalf("pending = %d, want the job waiting for its backoff", pending)
	}
	// Inside the backoff window nothing is claimable.
	if processed, _ := worker.ProcessBatch(context.Background()); processed != 0 {
		t.Fatal("a job was claimed during its backoff window")
	}

	for i := 0; i < 2; i++ {
		now = now.Add(10 * time.Minute)
		if _, err := worker.ProcessBatch(context.Background()); err != nil {
			t.Fatalf("ProcessBatch() error = %v", err)
		}
	}

	if attempts.Load() != 3 {
		t.Fatalf("handler ran %d times, want 3 (MaxAttempts)", attempts.Load())
	}
	if dead := count(t, jobType, domain.StatusDead); dead != 1 {
		t.Fatalf("dead jobs = %d, want 1: an exhausted job is parked, not retried forever", dead)
	}
}

func TestWorkerOnlyClaimsTypesItHandles(t *testing.T) {
	jobs, known := newQueue(t)
	unknown := testsupport.Unique("unknown")
	testsupport.CleanupJobs(t, testsupport.Database(t), unknown)
	worker := NewWorker(jobs)
	worker.Handle(known, func(context.Context, domain.Job) error { return nil })
	enqueue(t, jobs, &domain.Job{Type: known})
	enqueue(t, jobs, &domain.Job{Type: unknown})

	processed, err := worker.ProcessBatch(context.Background())

	if err != nil || processed != 1 {
		t.Fatalf("ProcessBatch() = (%d, %v), want only the known job", processed, err)
	}
	if pending := count(t, unknown, domain.StatusPending); pending != 1 {
		t.Fatalf("pending = %d, want the unknown job left for a worker that handles it", pending)
	}
}

func TestWorkerSurvivesAPanickingHandler(t *testing.T) {
	jobs, jobType := newQueue(t)
	worker := NewWorker(jobs)
	worker.Handle(jobType, func(context.Context, domain.Job) error {
		panic("nil map write")
	})
	enqueue(t, jobs, &domain.Job{Type: jobType, MaxAttempts: 2})

	// A panic must become a failed job, never a dead worker: one bad payload
	// would otherwise stop every payment in the queue.
	if _, err := worker.ProcessBatch(context.Background()); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if pending := count(t, jobType, domain.StatusPending); pending != 1 {
		t.Fatalf("pending = %d, want the job rescheduled after the panic", pending)
	}
}

func TestDedupeKeyCollapsesOpenDuplicatesOnly(t *testing.T) {
	jobs, jobType := newQueue(t)
	ctx := context.Background()
	key := testsupport.Unique("payment.sync")

	if !tryEnqueue(t, jobs, &domain.Job{Type: jobType, DedupeKey: key}) {
		t.Fatalf("Enqueue() = false for the first job, want added")
	}
	for i := 0; i < 5; i++ {
		// Each duplicate is answered "already scheduled" with no error: the
		// conflict is decided by the statement itself, never by a failure.
		if tryEnqueue(t, jobs, &domain.Job{Type: jobType, DedupeKey: key}) {
			t.Fatalf("Enqueue() = true for a duplicate, want it absorbed")
		}
	}
	if pending := count(t, jobType, domain.StatusPending); pending != 1 {
		t.Fatalf("pending = %d, want 1: five redeliveries of one event are one read", pending)
	}

	// Once the first is done the key is free again — the partial unique index
	// covers open jobs only, so a later event for the same payment is not
	// swallowed forever.
	worker := NewWorker(jobs)
	worker.Handle(jobType, func(context.Context, domain.Job) error { return nil })
	if _, err := worker.ProcessBatch(ctx); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	enqueue(t, jobs, &domain.Job{Type: jobType, DedupeKey: key})
	if pending := count(t, jobType, domain.StatusPending); pending != 1 {
		t.Fatalf("pending = %d, want the later event scheduled", pending)
	}
}

// TestDuplicateWhileRunningRerunsTheJobOnce is the race the rerun flag
// exists for: the running attempt may have read the provider before the event
// the duplicate announces, so the job runs again — once — instead of finishing.
func TestDuplicateWhileRunningRerunsTheJobOnce(t *testing.T) {
	jobs, jobType := newQueue(t)
	ctx := context.Background()
	key := testsupport.Unique("payment.sync")

	first := enqueue(t, jobs, &domain.Job{Type: jobType, DedupeKey: key})
	claimed, err := jobs.Claim(ctx, "worker-a", []string{jobType}, 1, time.Now())
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("Claim() = %v, %v; want the job", claimed, err)
	}

	// The duplicate arrives while the job is running.
	if tryEnqueue(t, jobs, &domain.Job{Type: jobType, DedupeKey: key}) {
		t.Fatalf("Enqueue() = true while the key is running, want it to flag the running job instead")
	}
	if pending := count(t, jobType, domain.StatusPending); pending != 0 {
		t.Fatalf("pending = %d, want 0: the duplicate must not become a second job", pending)
	}

	// Completing the flagged job re-queues it, with its attempt budget fresh.
	if err := jobs.Complete(ctx, first.ID, time.Now()); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	again, err := jobs.Claim(ctx, "worker-b", []string{jobType}, 1, time.Now())
	if err != nil || len(again) != 1 {
		t.Fatalf("Claim() after a flagged completion = %v, %v; want the same job back", again, err)
	}
	if again[0].ID != first.ID || again[0].Attempts != 1 || again[0].Rerun {
		t.Fatalf("rerun job = %+v, want the same id, attempts reset and the flag cleared", again[0])
	}

	// The second completion is final: the flag was consumed, not kept.
	if err := jobs.Complete(ctx, first.ID, time.Now()); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if done := count(t, jobType, domain.StatusDone); done != 1 {
		t.Fatalf("done = %d, want 1", done)
	}
	if pending := count(t, jobType, domain.StatusPending); pending != 0 {
		t.Fatalf("pending = %d, want 0 after the rerun completed", pending)
	}

	// The key is free again; a later event is a new job.
	if !tryEnqueue(t, jobs, &domain.Job{Type: jobType, DedupeKey: key}) {
		t.Fatalf("Enqueue() = false after the job finished, want the key free")
	}
}

// TestConcurrentWorkersNeverShareAJob is what FOR UPDATE SKIP LOCKED buys.
func TestConcurrentWorkersNeverShareAJob(t *testing.T) {
	jobs, jobType := newQueue(t)
	const total = 40
	for i := 0; i < total; i++ {
		enqueue(t, jobs, &domain.Job{Type: jobType})
	}

	var runs atomic.Int32
	seen := sync.Map{}
	var duplicates atomic.Int32
	handler := func(_ context.Context, job domain.Job) error {
		runs.Add(1)
		if _, loaded := seen.LoadOrStore(job.ID, true); loaded {
			duplicates.Add(1)
		}
		return nil
	}

	var wait sync.WaitGroup
	for w := 0; w < 4; w++ {
		worker := NewWorker(jobs, WithBatchSize(5))
		worker.Handle(jobType, handler)
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				processed, err := worker.ProcessBatch(context.Background())
				if err != nil {
					t.Errorf("ProcessBatch() error = %v", err)
					return
				}
				if processed == 0 {
					return
				}
			}
		}()
	}
	wait.Wait()

	if runs.Load() != total {
		t.Fatalf("handlers ran %d times, want %d", runs.Load(), total)
	}
	if duplicates.Load() != 0 {
		t.Fatalf("%d job(s) ran on two workers at once", duplicates.Load())
	}
	if done := count(t, jobType, domain.StatusDone); done != total {
		t.Fatalf("done = %d, want %d", done, total)
	}
}

func TestReclaimStaleReturnsAbandonedJobs(t *testing.T) {
	jobs, jobType := newQueue(t)
	ctx := context.Background()
	enqueue(t, jobs, &domain.Job{Type: jobType})

	if _, err := jobs.Claim(ctx, "worker-that-died", []string{jobType}, 1, time.Now()); err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if pending := count(t, jobType, domain.StatusPending); pending != 0 {
		t.Fatal("the job should be locked by the dead worker")
	}

	// Other packages may hold stale-looking rows of their own; only this
	// test's type is asserted on.
	reclaimed, err := jobs.ReclaimStale(ctx, time.Now().Add(time.Minute), time.Now())

	if err != nil || reclaimed < 1 {
		t.Fatalf("ReclaimStale() = (%d, %v), want at least 1", reclaimed, err)
	}
	if pending := count(t, jobType, domain.StatusPending); pending != 1 {
		t.Fatalf("pending = %d, want the job claimable again", pending)
	}
}

func TestClaimByIDRunsAJobExactlyOnce(t *testing.T) {
	jobs, jobType := newQueue(t)
	job := enqueue(t, jobs, &domain.Job{Type: jobType})
	worker := NewWorker(jobs)
	var ran atomic.Int32
	worker.Handle(jobType, func(context.Context, domain.Job) error {
		ran.Add(1)
		return nil
	})

	// The same broker message, delivered five times at once.
	var wait sync.WaitGroup
	for i := 0; i < 5; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := worker.ProcessMessage(context.Background(), domain.Message{JobID: job.ID, Type: job.Type}); err != nil {
				t.Errorf("ProcessMessage() error = %v", err)
			}
		}()
	}
	wait.Wait()

	if ran.Load() != 1 {
		t.Fatalf("handler ran %d times for one job, want exactly 1", ran.Load())
	}
}

func TestClaimByIDIgnoresAJobThatIsNotDueYet(t *testing.T) {
	jobs, jobType := newQueue(t)
	job := enqueue(t, jobs, &domain.Job{Type: jobType, RunAt: time.Now().Add(time.Hour)})
	worker := NewWorker(jobs)
	var ran atomic.Int32
	worker.Handle(jobType, func(context.Context, domain.Job) error {
		ran.Add(1)
		return nil
	})

	if err := worker.ProcessMessage(context.Background(), domain.Message{JobID: job.ID, Type: job.Type}); err != nil {
		t.Fatalf("ProcessMessage() error = %v", err)
	}

	if ran.Load() != 0 {
		t.Fatal("a job scheduled for later ran because a message named it")
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	first := domain.Backoff(1)
	second := domain.Backoff(2)
	far := domain.Backoff(50)

	if second <= first {
		t.Fatalf("backoff did not grow: %s then %s", first, second)
	}
	if far > 5*time.Minute {
		t.Fatalf("backoff = %s, want it capped: a buyer watching for a PIX code cannot wait an hour", far)
	}
}

func TestSchedulerEnqueuesOneSweepPerPeriod(t *testing.T) {
	jobs, _ := newQueue(t)
	scheduler := NewScheduler(jobs, NewDispatcher(nil), time.Minute)
	db := testsupport.Database(t)
	// A day far in the past, so no other test and no real tick shares a bucket.
	t.Cleanup(func() { db.Exec("DELETE FROM jobs WHERE dedupe_key LIKE ?", "%:sweep:20010913T%") })

	at := func(hour, minute int) time.Time {
		return time.Date(2001, 9, 13, hour, minute, 30, 0, time.UTC)
	}
	scheduled := func(jobType string) int64 {
		var total int64
		db.Raw("SELECT COUNT(*) FROM jobs WHERE dedupe_key LIKE ? AND status = 'pending'",
			jobType+":sweep:20010913T%").Scan(&total)
		return total
	}

	// Many instances ticking in the same period produce one sweep each, not one
	// per instance.
	scheduler.now = func() time.Time { return at(12, 0) }
	for i := 0; i < 4; i++ {
		scheduler.Tick(context.Background())
	}

	for _, jobType := range []string{domain.TypeExpireHolds, domain.TypeReconcile, domain.TypeCleanup, domain.TypeAuditSettled} {
		if got := scheduled(jobType); got != 1 {
			t.Fatalf("%s sweeps = %d after four ticks in one minute, want 1", jobType, got)
		}
	}

	// Forty-five minutes later, still inside the same hour. The minute sweeps
	// are due again; the settled-order audit is not.
	//
	// The cadences differ because the work does. Releasing a lapsed hold and
	// recovering a lost notification are minutes-matter jobs with a buyer
	// waiting. Re-reading orders that already settled catches a refund
	// notification nobody received — running that every minute would spend a
	// provider call per paid order per minute to catch something that happens a
	// few times a day.
	scheduler.now = func() time.Time { return at(12, 45) }
	scheduler.Tick(context.Background())

	for _, jobType := range []string{domain.TypeExpireHolds, domain.TypeReconcile, domain.TypeCleanup} {
		if got := scheduled(jobType); got != 2 {
			t.Fatalf("%s sweeps = %d in a new minute, want 2", jobType, got)
		}
	}
	if got := scheduled(domain.TypeAuditSettled); got != 1 {
		t.Fatalf("settled-order audits = %d within one hour, want 1: the hourly sweep ran on a minute cadence", got)
	}

	// The next hour turns it over.
	scheduler.now = func() time.Time { return at(13, 5) }
	scheduler.Tick(context.Background())

	if got := scheduled(domain.TypeAuditSettled); got != 2 {
		t.Fatalf("settled-order audits = %d in a new hour, want 2", got)
	}
}

func TestCompletedJobRetentionIsBoundedAndPreservesAuditFailures(t *testing.T) {
	jobs, jobType := newQueue(t)
	db := testsupport.Database(t)
	now := time.Now().UTC()

	oldIDs := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		job := enqueue(t, jobs, &domain.Job{Type: jobType, Status: domain.StatusDone})
		oldIDs = append(oldIDs, job.ID)
	}
	recent := enqueue(t, jobs, &domain.Job{Type: jobType, Status: domain.StatusDone})
	dead := enqueue(t, jobs, &domain.Job{Type: jobType, Status: domain.StatusDead})
	if err := db.Exec("UPDATE jobs SET updated_at = ? WHERE id IN ?", now.Add(-48*time.Hour), oldIDs).Error; err != nil {
		t.Fatalf("age completed jobs: %v", err)
	}

	deleted, err := jobs.DeleteCompletedBefore(context.Background(), now.Add(-24*time.Hour), 2)
	if err != nil || deleted != 2 {
		t.Fatalf("DeleteCompletedBefore() = (%d, %v), want (2, nil)", deleted, err)
	}
	if got := count(t, jobType, domain.StatusDone); got != 2 {
		t.Fatalf("done jobs after bounded delete = %d, want one old and one recent", got)
	}
	if got := count(t, jobType, domain.StatusDead); got != 1 {
		t.Fatalf("dead jobs after cleanup = %d, want preserved", got)
	}

	deleted, err = jobs.DeleteCompletedBefore(context.Background(), now.Add(-24*time.Hour), 10)
	if err != nil || deleted != 1 {
		t.Fatalf("second DeleteCompletedBefore() = (%d, %v), want (1, nil)", deleted, err)
	}
	var survivors int64
	db.Raw("SELECT COUNT(*) FROM jobs WHERE id IN ?", []string{recent.ID, dead.ID}).Scan(&survivors)
	if survivors != 2 {
		t.Fatalf("recent/dead survivors = %d, want 2", survivors)
	}
}

// TestBrokerDeliversAPublishedJobToAWorker is the end-to-end transport test:
// a job written to PostgreSQL, announced on RabbitMQ, consumed by a worker,
// claimed in PostgreSQL and completed.
func TestBrokerDeliversAPublishedJobToAWorker(t *testing.T) {
	url := testsupport.RabbitMQURL(t)
	jobs, jobType := newQueue(t)

	broker, err := rabbitmq.Connect(url, rabbitmq.WithPrefetch(4),
		rabbitmq.WithNamespace(testsupport.Unique("broker")), rabbitmq.WithEphemeralTopology())
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })

	ran := make(chan string, 8)
	worker := NewWorker(jobs, WithBroker(broker), WithInterval(time.Hour))
	worker.Handle(jobType, func(_ context.Context, job domain.Job) error {
		ran <- job.ID
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go worker.Run(ctx)
	// The consumer needs a moment to attach before the first publish.
	time.Sleep(500 * time.Millisecond)

	job := enqueue(t, jobs, &domain.Job{Type: jobType})
	NewDispatcher(broker).Dispatch(ctx, job)

	select {
	case got := <-ran:
		if got != job.ID {
			t.Fatalf("worker ran %s, want %s", got, job.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the job never reached the worker through the broker")
	}

	// Redelivered by the broker — or published twice — the same message must
	// not run the job again.
	NewDispatcher(broker).Dispatch(ctx, job)
	select {
	case got := <-ran:
		t.Fatalf("job %s ran a second time on redelivery", got)
	case <-time.After(1500 * time.Millisecond):
	}

	deadline := time.Now().Add(5 * time.Second)
	for count(t, jobType, domain.StatusDone) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the job was run but never marked done")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBrokerRunsDeliveriesConcurrently is the throughput property: eight
// slow jobs delivered through the broker must run together, not one after
// another. Sequential would take eight times the handler's duration.
func TestBrokerRunsDeliveriesConcurrently(t *testing.T) {
	url := testsupport.RabbitMQURL(t)
	jobs, jobType := newQueue(t)

	broker, err := rabbitmq.Connect(url, rabbitmq.WithPrefetch(8),
		rabbitmq.WithNamespace(testsupport.Unique("broker")), rabbitmq.WithEphemeralTopology())
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })

	const slow = 300 * time.Millisecond
	finished := make(chan struct{}, 8)
	worker := NewWorker(jobs, WithBroker(broker), WithInterval(time.Hour))
	worker.Handle(jobType, func(context.Context, domain.Job) error {
		time.Sleep(slow)
		finished <- struct{}{}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go worker.Run(ctx)
	time.Sleep(500 * time.Millisecond) // the consumer attaches

	started := time.Now()
	dispatcher := NewDispatcher(broker)
	for i := 0; i < 8; i++ {
		dispatcher.Dispatch(ctx, enqueue(t, jobs, &domain.Job{Type: jobType}))
	}
	for i := 0; i < 8; i++ {
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of 8 deliveries ran", i)
		}
	}
	if took := time.Since(started); took > 4*slow {
		t.Fatalf("8 deliveries took %s; in flight together they take about %s, one at a time %s", took, slow, 8*slow)
	}

	deadline := time.Now().Add(5 * time.Second)
	for count(t, jobType, domain.StatusDone) != 8 {
		if time.Now().After(deadline) {
			t.Fatalf("done = %d, want all 8 completed", count(t, jobType, domain.StatusDone))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestPollerRunsABatchConcurrently is the same property on the fallback
// path: a claimed batch runs together, and ProcessBatch returns once the
// whole batch is settled.
func TestPollerRunsABatchConcurrently(t *testing.T) {
	jobs, jobType := newQueue(t)
	for i := 0; i < 8; i++ {
		enqueue(t, jobs, &domain.Job{Type: jobType})
	}

	const slow = 300 * time.Millisecond
	worker := NewWorker(jobs, WithBatchSize(8))
	worker.Handle(jobType, func(context.Context, domain.Job) error {
		time.Sleep(slow)
		return nil
	})

	started := time.Now()
	processed, err := worker.ProcessBatch(context.Background())
	if err != nil || processed != 8 {
		t.Fatalf("ProcessBatch() = %d, %v; want 8 jobs", processed, err)
	}
	if took := time.Since(started); took > 4*slow {
		t.Fatalf("a batch of 8 took %s; together they take about %s, one at a time %s", took, slow, 8*slow)
	}
	if done := count(t, jobType, domain.StatusDone); done != 8 {
		t.Fatalf("done = %d, want the whole batch completed before ProcessBatch returned", done)
	}
}

func TestBrokerCorrelatesConcurrentPublisherConfirmations(t *testing.T) {
	url := testsupport.RabbitMQURL(t)
	broker, err := rabbitmq.Connect(url,
		rabbitmq.WithNamespace(testsupport.Unique("broker")), rabbitmq.WithEphemeralTopology())
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = broker.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const total = 64
	errorsSeen := make(chan error, total)
	var wait sync.WaitGroup
	for index := 0; index < total; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if err := broker.Publish(ctx, domain.Message{JobID: fmt.Sprintf("publish-%d", index), Type: "test"}); err != nil {
				errorsSeen <- err
			}
		}(index)
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent Publish() error = %v", err)
	}
	if depth, err := broker.QueueDepth(); err != nil || depth != total {
		t.Fatalf("QueueDepth() = (%d, %v), want %d individually confirmed messages", depth, err, total)
	}
}

type blockingPublisher struct {
	started atomic.Int32
	release chan struct{}
}

func (p *blockingPublisher) Publish(ctx context.Context, _ domain.Message) error {
	p.started.Add(1)
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestDispatcherBoundsAsyncBrokerWork(t *testing.T) {
	publisher := &blockingPublisher{release: make(chan struct{})}
	dispatcher := NewDispatcher(publisher)
	job := &domain.Job{ID: "job", Type: "test", RunAt: time.Now()}

	started := time.Now()
	for index := 0; index < maxDispatchInFlight*4; index++ {
		job.ID = fmt.Sprintf("job-%d", index)
		dispatcher.Dispatch(context.Background(), job)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("Dispatch blocked request processing for %s", took)
	}

	deadline := time.Now().Add(time.Second)
	for publisher.started.Load() < maxDispatchInFlight && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := publisher.started.Load(); got != maxDispatchInFlight {
		t.Fatalf("publisher calls in flight = %d, want bounded at %d", got, maxDispatchInFlight)
	}
	close(publisher.release)
}

// TestAJobInHandSurvivesShutdown is the deploy case: a worker told to stop
// mid-job must finish that job and record it, not abandon it for the stale
// sweep to run again half an hour later.
func TestAJobInHandSurvivesShutdown(t *testing.T) {
	jobs, jobType := newQueue(t)

	started := make(chan struct{})
	finished := make(chan error, 1)
	worker := NewWorker(jobs, WithInterval(50*time.Millisecond))
	worker.Handle(jobType, func(ctx context.Context, _ domain.Job) error {
		close(started)
		time.Sleep(400 * time.Millisecond)
		// The handler's own context must still be usable: it is what the
		// charge call and every repository write inside the job use.
		finished <- ctx.Err()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	enqueue(t, jobs, &domain.Job{Type: jobType})
	go worker.Run(ctx)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the job never started")
	}
	cancel() // the deploy begins while the job is in flight

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("the handler's context was cancelled mid-job: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never finished")
	}

	deadline := time.Now().Add(5 * time.Second)
	for count(t, jobType, domain.StatusDone) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("job is %s, want done: a shutdown must not leave finished work marked processing",
				statusOf(t, jobType))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTimedOutJobIsRecordedForRetry(t *testing.T) {
	jobs, jobType := newQueue(t)
	worker := NewWorker(jobs, WithJobTimeout(50*time.Millisecond))
	worker.Handle(jobType, func(ctx context.Context, _ domain.Job) error {
		<-ctx.Done()
		return ctx.Err()
	})
	enqueue(t, jobs, &domain.Job{Type: jobType, MaxAttempts: 2})

	if processed, err := worker.ProcessBatch(context.Background()); err != nil || processed != 1 {
		t.Fatalf("ProcessBatch() = (%d, %v), want one timed-out attempt", processed, err)
	}
	if pending := count(t, jobType, domain.StatusPending); pending != 1 {
		t.Fatalf("pending = %d, want the timed-out job recorded for retry; status is %s", pending, statusOf(t, jobType))
	}
}

// statusOf reports where a test's single job ended up, for failure messages.
func statusOf(t *testing.T, jobType string) string {
	t.Helper()
	for _, status := range []domain.Status{domain.StatusPending, domain.StatusProcessing, domain.StatusDone, domain.StatusDead} {
		if count(t, jobType, status) > 0 {
			return string(status)
		}
	}
	return "gone"
}

func TestBrokerRefusesToPublishWhenClosed(t *testing.T) {
	url := testsupport.RabbitMQURL(t)
	broker, err := rabbitmq.Connect(url,
		rabbitmq.WithNamespace(testsupport.Unique("broker")), rabbitmq.WithEphemeralTopology())
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if err := broker.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// A publish that cannot be confirmed must say so, so the caller knows the
	// poller is now the only delivery path.
	if err := broker.Publish(context.Background(), domain.Message{JobID: "x", Type: "y"}); err == nil {
		t.Fatal("Publish() on a closed broker returned nil")
	}
}
