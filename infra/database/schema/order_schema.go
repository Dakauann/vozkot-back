package schema

import "time"

// Order is the persisted purchase.
//
// Money is integer centavos and the ticket reference is RESTRICTed rather than
// cascaded: deleting a tier that has orders would delete the record of money
// that changed hands, so the database refuses and the API explains why.
type Order struct {
	ID       string `gorm:"primaryKey;type:varchar(32)"`
	TicketID string `gorm:"not null;type:varchar(32);index:idx_orders_ticket_id"`
	BuyerID  string `gorm:"not null;type:varchar(32);default:'';index:idx_orders_buyer_id"`

	BuyerName     string `gorm:"not null;type:varchar(255)"`
	BuyerEmail    string `gorm:"not null;type:varchar(320);index:idx_orders_buyer_email"`
	BuyerDocument string `gorm:"not null;type:varchar(20);default:''"`

	Quantity       int    `gorm:"not null"`
	UnitPriceCents int64  `gorm:"not null;default:0"`
	TotalCents     int64  `gorm:"not null;default:0"`
	Currency       string `gorm:"not null;type:varchar(3);default:'BRL'"`

	Status string `gorm:"not null;type:varchar(32);index:idx_orders_status"`
	// HoldExpiresAt is indexed because the expiry sweep queries exactly this
	// column, every minute, forever.
	HoldExpiresAt time.Time `gorm:"not null;index:idx_orders_hold_expires_at"`

	PaymentProvider string `gorm:"not null;type:varchar(32);default:''"`
	// PaymentID is unique per provider: two orders can never point at the same
	// charge, which is what makes "find the order this webhook is about"
	// unambiguous.
	// Uniqueness is enforced by a PARTIAL index the migration creates, covering
	// only rows that have a charge; a plain unique index here would allow one
	// chargeless order per provider.
	PaymentID       string `gorm:"not null;type:varchar(64);default:'';index:idx_orders_payment_id"`
	PaymentStatus   string `gorm:"not null;type:varchar(32);default:''"`
	PaymentMethod   string `gorm:"not null;type:varchar(32);default:''"`
	PixCopyPaste    string `gorm:"type:text;not null;default:''"`
	PixQRCodeBase64 string `gorm:"type:text;not null;default:''"`

	// IdempotencyKey is a pointer so it can be NULL: PostgreSQL allows many
	// NULLs under a unique index but only one empty string, and orders created
	// without a key (an operator selling at the door) must not collide.
	IdempotencyKey *string `gorm:"type:varchar(255);uniqueIndex:idx_orders_idempotency_key"`

	PaidAt    *time.Time
	ClosedAt  *time.Time
	CreatedAt time.Time `gorm:"not null;autoCreateTime;index:idx_orders_created_at"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime"`

	Ticket Ticket `gorm:"foreignKey:TicketID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

func (Order) TableName() string { return "orders" }
