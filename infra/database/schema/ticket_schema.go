package schema

import "time"

// Ticket is the persisted shape of a box office ticket tier.
//
// Prices are integer centavos. A float would be the obvious choice and the
// wrong one: two of them added together stop agreeing with the invoice.
type Ticket struct {
	ID      string `gorm:"primaryKey;type:varchar(32)"`
	OwnerID string `gorm:"not null;type:varchar(32);index:idx_tickets_owner_id"`
	// EventID is the happening this tier belongs to.
	//
	// RESTRICT rather than CASCADE: deleting an event that still has tiers
	// would delete tiers that may have orders against them, and those orders
	// are the record of money that changed hands. The API explains the refusal
	// instead.
	//
	// Declared `not null` because that is what the table actually is once the
	// migration has run, and the two MUST agree: a struct that says nullable
	// against a column that is not sends AutoMigrate into altering the column
	// on every single boot, which is DDL in steady state, an ACCESS EXCLUSIVE
	// lock taken against live traffic, forever. It deadlocked a test run that
	// way before this comment existed.
	//
	// AutoMigrate cannot ADD a NOT NULL column to a table that already has
	// rows, so the migration adds it nullable, backfills an event for every
	// tier and applies the constraint BEFORE AutoMigrate ever sees this tag.
	EventID string `gorm:"not null;type:varchar(32);index:idx_tickets_event_id"`
	// EventName, Venue, City and StartsAt now live on the event. They are kept
	// here, no longer indexed and no longer read, only so a deploy can be rolled
	// back without losing the values the backfill copied up. A later migration
	// drops them; until then nothing writes them.
	//
	// All four are NULLABLE, and the tags have to say so. The migration drops
	// their NOT NULL because a tier no longer supplies them; a tag that still
	// claimed `not null` would send AutoMigrate into putting the constraint
	// back on every boot, undoing the migration and taking a table lock against
	// live traffic to do it.
	EventName   string `gorm:"type:varchar(255)"`
	Title       string `gorm:"not null;type:varchar(255)"`
	Description string `gorm:"type:text;not null;default:''"`
	Venue       string `gorm:"type:varchar(255)"`
	City        string `gorm:"type:varchar(255)"`
	StartsAt    *time.Time
	PriceCents  int64  `gorm:"not null;default:0"`
	Currency    string `gorm:"not null;type:varchar(3);default:'BRL'"`
	Quantity    int    `gorm:"not null;default:0"`
	Sold        int    `gorm:"not null;default:0"`
	// Reserved is stock held by orders awaiting payment: neither sold nor
	// available. The CHECK constraint in the migration keeps
	// sold + reserved within quantity whatever the application does.
	Reserved  int       `gorm:"not null;default:0"`
	Status    string    `gorm:"not null;type:varchar(32);index:idx_tickets_status;default:'draft'"`
	CreatedAt time.Time `gorm:"not null;autoCreateTime;index:idx_tickets_created_at"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime"`

	Owner User  `gorm:"foreignKey:OwnerID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	Event Event `gorm:"foreignKey:EventID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

func (Ticket) TableName() string { return "tickets" }
