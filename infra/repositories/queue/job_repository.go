package queue

import (
	"context"
	"strings"
	"time"

	domain "vozkot/domain/queue"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
)

// JobRepository is the durable work ledger built on one table.
//
// A job and the order it refers to commit in the SAME transaction. RabbitMQ
// delivers the reference quickly; the row is the source of truth if publishing
// is interrupted or the broker is unavailable.
type JobRepository struct {
	db *gorm.DB
}

func NewJobRepository(db *gorm.DB) *JobRepository {
	return &JobRepository{db: db}
}

// Enqueue adds a job and reports whether it did.
//
// Deduplication is one statement, and the statement never fails: the INSERT
// names the partial unique index over open jobs as its conflict target, so a
// duplicate is decided by the database without an error, a rolled-back
// transaction, or a line in its log. That is not cosmetic. A provider that
// delivers every webhook three times would otherwise fail two inserts in
// three, and a failure that is expected is not a failure — it is control flow,
// and control flow does not belong in the error log at a million events.
//
// What a duplicate does depends on the job it found:
//
//   - still pending: dropped. The one waiting will read the latest state.
//   - already running: flagged to run again. The running attempt may have read
//     the provider a moment BEFORE the event this duplicate announces, and
//     finishing it would lose that event until a sweep happened to look.
//
// xmax is how PostgreSQL tells the two outcomes apart in one RETURNING: a row
// this statement inserted has none, a row it updated has one.
func (q *JobRepository) Enqueue(ctx context.Context, job *domain.Job) (bool, error) {
	record := toSchema(job)
	if record.Status == "" {
		record.Status = string(domain.StatusPending)
	}
	if record.MaxAttempts <= 0 {
		record.MaxAttempts = domain.DefaultMaxAttempts
	}
	if len(record.Payload) == 0 {
		record.Payload = []byte(`{}`)
	}
	timestamp := time.Now().UTC()
	if record.RunAt.IsZero() {
		record.RunAt = timestamp
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = timestamp
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = timestamp
	}

	var outcome struct {
		ID       string
		Inserted bool
	}
	err := q.db.WithContext(ctx).Raw(`
		INSERT INTO jobs (id, type, payload, status, attempts, max_attempts, run_at, last_error,
		                  dedupe_key, rerun, locked_by, locked_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, FALSE, ?, ?, ?, ?)
		ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('pending', 'processing')
		DO UPDATE SET rerun = TRUE, updated_at = EXCLUDED.updated_at
		WHERE jobs.status = 'processing'
		RETURNING id, (xmax = 0) AS inserted`,
		record.ID, record.Type, record.Payload, record.Status, record.Attempts, record.MaxAttempts,
		record.RunAt.UTC(), record.LastError, record.DedupeKey, record.LockedBy, record.LockedAt,
		record.CreatedAt.UTC(), record.UpdatedAt.UTC(),
	).Scan(&outcome).Error
	if err != nil {
		return false, err
	}
	// No row: the duplicate met a pending job and was dropped. A row that is not
	// ours: it met a running job and flagged it. Only our own row means added.
	if !outcome.Inserted || outcome.ID != record.ID {
		return false, nil
	}
	*job = *toDomain(&record)
	return true, nil
}

// Claim locks due jobs for one worker.
//
// FOR UPDATE SKIP LOCKED is the whole reason this works with many workers: rows
// another worker is holding are stepped over rather than waited on, so N
// workers process N jobs at once instead of queueing behind each other.
func (q *JobRepository) Claim(ctx context.Context, workerID string, types []string, limit int, now time.Time) ([]domain.Job, error) {
	if len(types) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	timestamp := now.UTC()

	var records []schema.Job
	err := q.db.WithContext(ctx).Raw(`
		UPDATE jobs
		SET status = ?, attempts = attempts + 1, locked_by = ?, locked_at = ?, updated_at = ?
		WHERE id IN (
			SELECT id FROM jobs
			WHERE status = ? AND run_at <= ? AND type IN ?
			ORDER BY run_at
			LIMIT ?
			FOR UPDATE SKIP LOCKED
		)
		RETURNING *`,
		string(domain.StatusProcessing), workerID, timestamp, timestamp,
		string(domain.StatusPending), timestamp, types, limit,
	).Scan(&records).Error
	if err != nil {
		return nil, err
	}

	jobs := make([]domain.Job, 0, len(records))
	for index := range records {
		jobs = append(jobs, *toDomain(&records[index]))
	}
	return jobs, nil
}

// ClaimByID locks the one job a broker delivery names.
//
// The WHERE clause is the deduplication: a message redelivered after the job
// ran finds status 'done' and matches no row, so the duplicate costs one
// indexed update that changes nothing.
func (q *JobRepository) ClaimByID(ctx context.Context, workerID, jobID string, now time.Time) (*domain.Job, bool, error) {
	timestamp := now.UTC()

	var records []schema.Job
	err := q.db.WithContext(ctx).Raw(`
		UPDATE jobs
		SET status = ?, attempts = attempts + 1, locked_by = ?, locked_at = ?, updated_at = ?
		WHERE id = ? AND status = ? AND run_at <= ?
		RETURNING *`,
		string(domain.StatusProcessing), workerID, timestamp, timestamp,
		jobID, string(domain.StatusPending), timestamp,
	).Scan(&records).Error
	if err != nil {
		return nil, false, err
	}
	if len(records) == 0 {
		return nil, false, nil
	}
	return toDomain(&records[0]), true, nil
}

// Complete finishes a claimed job — unless a duplicate was enqueued while it
// ran, in which case the job goes straight back to pending for one more read
// of state that is now newer than what this run saw. The attempt counter
// starts over: that is new work, not a failure.
func (q *JobRepository) Complete(ctx context.Context, jobID string, now time.Time) error {
	timestamp := now.UTC()
	result := q.db.WithContext(ctx).Exec(`
		UPDATE jobs
		SET status     = CASE WHEN rerun THEN ? ELSE ? END,
		    run_at     = CASE WHEN rerun THEN ? ELSE run_at END,
		    attempts   = CASE WHEN rerun THEN 0 ELSE attempts END,
		    rerun      = FALSE,
		    locked_by  = '',
		    locked_at  = NULL,
		    last_error = '',
		    updated_at = ?
		WHERE id = ?`,
		string(domain.StatusPending), string(domain.StatusDone), timestamp, timestamp, jobID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Retry clears the rerun flag as well: the retry runs later than the event
// that set it and reads the state that event produced.
func (q *JobRepository) Retry(ctx context.Context, jobID, reason string, runAt, now time.Time) error {
	return q.transition(ctx, jobID, map[string]any{
		"status":     string(domain.StatusPending),
		"run_at":     runAt.UTC(),
		"rerun":      false,
		"last_error": truncate(reason, 2000),
		"locked_by":  "",
		"locked_at":  nil,
		"updated_at": now.UTC(),
	})
}

func (q *JobRepository) Kill(ctx context.Context, jobID, reason string, now time.Time) error {
	return q.transition(ctx, jobID, map[string]any{
		"status":     string(domain.StatusDead),
		"rerun":      false,
		"last_error": truncate(reason, 2000),
		"locked_by":  "",
		"locked_at":  nil,
		"updated_at": now.UTC(),
	})
}

// ReclaimStale returns jobs whose worker died back to the queue.
//
// A process killed mid-job leaves a row marked processing forever. The
// attempts counter is already spent, so a reclaimed job resumes its retry
// budget rather than restarting it — a job that reliably kills its worker gets
// parked instead of looping.
func (q *JobRepository) ReclaimStale(ctx context.Context, olderThan, now time.Time) (int, error) {
	result := q.db.WithContext(ctx).Model(&schema.Job{}).
		Where("status = ? AND locked_at IS NOT NULL AND locked_at < ?",
			string(domain.StatusProcessing), olderThan.UTC()).
		Updates(map[string]any{
			"status":     string(domain.StatusPending),
			"locked_by":  "",
			"locked_at":  nil,
			"last_error": "worker did not report back; job reclaimed",
			"updated_at": now.UTC(),
		})
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

// CountOutstanding is how much work the system still owes: jobs that are
// running, and jobs that are pending — including those waiting out a retry
// backoff, whose run_at is in the future but whose work is not done.
//
// The one exception is a hold expiry scheduled for later: that is not work the
// system is behind on, it is an appointment, and counting it would make a
// queue with thirty-minute holds look permanently busy.
func (q *JobRepository) CountOutstanding(ctx context.Context, now time.Time) int64 {
	var total int64
	q.db.WithContext(ctx).Model(&schema.Job{}).
		Where("status = ? OR (status = ? AND NOT (type = ? AND run_at > ?))",
			string(domain.StatusProcessing), string(domain.StatusPending),
			domain.TypeExpireHolds, now.UTC()).
		Count(&total)
	return total
}

func (q *JobRepository) CountByStatus(ctx context.Context, status domain.Status) (int64, error) {
	var total int64
	if err := q.db.WithContext(ctx).Model(&schema.Job{}).
		Where("status = ?", string(status)).Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// DeleteCompletedBefore applies retention without ever deleting unfinished or
// dead work. The CTE bounds each transaction and SKIP LOCKED lets maintenance
// coexist with another replica doing the same cleanup during a rolling deploy.
func (q *JobRepository) DeleteCompletedBefore(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	result := q.db.WithContext(ctx).Exec(`
		WITH victims AS (
			SELECT id FROM jobs
			WHERE status = ? AND updated_at < ?
			ORDER BY updated_at, id
			LIMIT ?
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM jobs
		USING victims
		WHERE jobs.id = victims.id`,
		string(domain.StatusDone), olderThan.UTC(), limit)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

func (q *JobRepository) transition(ctx context.Context, jobID string, values map[string]any) error {
	result := q.db.WithContext(ctx).Model(&schema.Job{}).Where("id = ?", jobID).Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func toSchema(job *domain.Job) schema.Job {
	var dedupe *string
	if trimmed := strings.TrimSpace(job.DedupeKey); trimmed != "" {
		dedupe = &trimmed
	}
	return schema.Job{
		ID:          job.ID,
		Type:        job.Type,
		Payload:     job.Payload,
		Status:      string(job.Status),
		Attempts:    job.Attempts,
		MaxAttempts: job.MaxAttempts,
		RunAt:       job.RunAt,
		LastError:   job.LastError,
		DedupeKey:   dedupe,
		Rerun:       job.Rerun,
		LockedBy:    job.LockedBy,
		LockedAt:    job.LockedAt,
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   job.UpdatedAt,
	}
}

func toDomain(record *schema.Job) *domain.Job {
	dedupe := ""
	if record.DedupeKey != nil {
		dedupe = *record.DedupeKey
	}
	return &domain.Job{
		ID:          record.ID,
		Type:        record.Type,
		Payload:     record.Payload,
		Status:      domain.Status(record.Status),
		Attempts:    record.Attempts,
		MaxAttempts: record.MaxAttempts,
		RunAt:       record.RunAt,
		LastError:   record.LastError,
		DedupeKey:   dedupe,
		Rerun:       record.Rerun,
		LockedBy:    record.LockedBy,
		LockedAt:    record.LockedAt,
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
	}
}
