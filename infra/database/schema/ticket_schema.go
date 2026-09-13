package schema

import "time"

// Ticket is the persisted shape of a box office ticket tier.
//
// Prices are integer centavos. A float would be the obvious choice and the
// wrong one: two of them added together stop agreeing with the invoice.
type Ticket struct {
	ID          string    `gorm:"primaryKey;type:varchar(32)"`
	OwnerID     string    `gorm:"not null;type:varchar(32);index:idx_tickets_owner_id"`
	EventName   string    `gorm:"not null;type:varchar(255);index:idx_tickets_event_name"`
	Title       string    `gorm:"not null;type:varchar(255)"`
	Description string    `gorm:"type:text;not null;default:''"`
	Venue       string    `gorm:"not null;type:varchar(255)"`
	City        string    `gorm:"type:varchar(255);not null;default:''"`
	StartsAt    time.Time `gorm:"not null;index:idx_tickets_starts_at"`
	PriceCents  int64     `gorm:"not null;default:0"`
	Currency    string    `gorm:"not null;type:varchar(3);default:'BRL'"`
	Quantity    int       `gorm:"not null;default:0"`
	Sold        int       `gorm:"not null;default:0"`
	// Reserved is stock held by orders awaiting payment: neither sold nor
	// available. The CHECK constraint in the migration keeps
	// sold + reserved within quantity whatever the application does.
	Reserved  int       `gorm:"not null;default:0"`
	Status    string    `gorm:"not null;type:varchar(32);index:idx_tickets_status;default:'draft'"`
	CreatedAt time.Time `gorm:"not null;autoCreateTime;index:idx_tickets_created_at"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime"`

	Owner User `gorm:"foreignKey:OwnerID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (Ticket) TableName() string { return "tickets" }
