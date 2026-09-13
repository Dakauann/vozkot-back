package schema

import "time"

// Job is one durable unit of work.
//
// The table is a queue, and the index below is what makes it one: claiming is a
// single ordered scan of (status, run_at) filtered by type, and PostgreSQL's
// SKIP LOCKED lets many workers walk it at once without ever handing the same
// row to two of them.
//
// The column order matters. Leading with status and run_at makes "pending jobs
// due now, oldest first" one range scan that stops at its LIMIT, with the type
// checked in the index. Leading with type would make it one scan per handled
// type plus a sort of the entire backlog on every claim — fine when the queue
// is empty, and exactly wrong during the burst a queue exists for.
type Job struct {
	ID      string `gorm:"primaryKey;type:varchar(48)"`
	Type    string `gorm:"not null;type:varchar(64);index:idx_jobs_claim,priority:3"`
	Payload []byte `gorm:"type:jsonb;not null;default:'{}'"`

	Status      string `gorm:"not null;type:varchar(16);default:'pending';index:idx_jobs_claim,priority:1"`
	Attempts    int    `gorm:"not null;default:0"`
	MaxAttempts int    `gorm:"not null;default:8"`
	// RunAt is both the schedule and the backoff: a retry is a row with a later
	// RunAt, never a sleeping goroutine.
	RunAt     time.Time `gorm:"not null;index:idx_jobs_claim,priority:2"`
	LastError string    `gorm:"type:text;not null;default:''"`

	// DedupeKey is nullable so that jobs without one do not all collide under
	// the partial unique index the migration adds.
	DedupeKey *string `gorm:"type:varchar(255)"`
	// Rerun is the "once more" flag a duplicate sets on a job it found already
	// running; see the repository's Enqueue.
	Rerun bool `gorm:"not null;default:false"`

	LockedBy string     `gorm:"not null;type:varchar(128);default:''"`
	LockedAt *time.Time `gorm:"index:idx_jobs_locked_at"`

	CreatedAt time.Time `gorm:"not null;autoCreateTime"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime"`
}

func (Job) TableName() string { return "jobs" }
