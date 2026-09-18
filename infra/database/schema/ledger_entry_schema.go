package schema

import "time"

// LedgerEntry is one movement on one organiser's balance.
//
// APPEND-ONLY, and nothing in the mapping suggests otherwise: there is no
// UpdatedAt, because a row that is never updated has no such moment, and its
// absence is the first thing that tells a reader what this table is.
//
// The order reference is deliberately NOT a foreign key. Every other money row
// RESTRICTs its order so the evidence cannot be deleted, and that is right for
// them, but this table outlives the catalogue on purpose. An organiser's
// balance has to stay reconcilable after an event and its orders are purged for
// data retention, and a constraint that blocked that purge would make the
// retention job impossible to run instead of making the ledger safer.
type LedgerEntry struct {
	ID string `gorm:"primaryKey;type:varchar(32)"`

	// OrganiserID is the subject of the whole table: whose money this is.
	//
	// The composite index with AvailableAt is the payable-balance query, the
	// one a payout sweep runs for every organiser, every day, forever, and it
	// carries AmountCents so that sum never touches the heap.
	OrganiserID string `gorm:"not null;type:varchar(32);index:idx_ledger_payable,priority:1"`
	// AvailableAt is when this money may leave. See domain/ledger.
	AvailableAt time.Time `gorm:"not null;index:idx_ledger_payable,priority:2"`
	AmountCents int64     `gorm:"not null"`

	// EventID answers "what did this show earn me", which is one indexed read
	// rather than a join through orders on every dashboard load.
	EventID string `gorm:"not null;type:varchar(32);default:'';index:idx_ledger_event"`
	// OrderID is empty on an adjustment or a payout, which belong to the
	// organiser rather than to any one sale.
	OrderID string `gorm:"not null;type:varchar(32);default:'';index:idx_ledger_order"`

	Kind string `gorm:"not null;type:varchar(16)"`
	Note string `gorm:"type:text;not null;default:''"`

	CreatedAt time.Time `gorm:"not null;index:idx_ledger_created_at"`
}

func (LedgerEntry) TableName() string { return "ledger_entries" }
