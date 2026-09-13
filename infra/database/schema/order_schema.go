package schema

import "time"

// Order is the persisted purchase: the buyer, the money, the hold and the
// charge. What was actually bought lives in OrderItem.
//
// Money is integer centavos. The event reference is RESTRICTed rather than
// cascaded: deleting an event that has orders would delete the record of money
// that changed hands, so the database refuses and the API explains why.
type Order struct {
	ID string `gorm:"primaryKey;type:varchar(32)"`
	// EventID is the one night this order is for. Every item belongs to it,
	// which is what lets a listing show the show without reading the tiers.
	EventID string `gorm:"not null;type:varchar(32);default:'';index:idx_orders_event_id"`
	BuyerID string `gorm:"not null;type:varchar(32);default:'';index:idx_orders_buyer_id"`

	BuyerName     string `gorm:"not null;type:varchar(255)"`
	BuyerEmail    string `gorm:"not null;type:varchar(320);index:idx_orders_buyer_email"`
	BuyerDocument string `gorm:"not null;type:varchar(20);default:''"`

	TotalCents int64  `gorm:"not null;default:0"`
	Currency   string `gorm:"not null;type:varchar(3);default:'BRL'"`

	Status string `gorm:"not null;type:varchar(32);index:idx_orders_status"`
	// HoldExpiresAt is indexed because the expiry sweep queries exactly this
	// column, every minute, forever.
	HoldExpiresAt time.Time `gorm:"not null;index:idx_orders_hold_expires_at"`
	// Confirmed separates a cart hold from an order somebody has asked to pay
	// for. It is what stops the charge job being created for a basket the buyer
	// is still filling in.
	Confirmed bool `gorm:"not null;default:false"`

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

	// No Event association, deliberately. Declaring one would have AutoMigrate
	// create the foreign key immediately — against a table whose existing rows
	// have not been backfilled yet, which fails every boot that has orders
	// predating the column. The constraint is added by enforceOrderEventLink
	// once every order has an event, and skipped while any does not.

	// The items constraint is declared HERE, on the has-many side, and not on a
	// belongs-to field over on OrderItem. Declaring it in both places makes
	// GORM emit two foreign keys over the same column, and the one it derives
	// from this side carries no ON DELETE — so deleting an order would be
	// refused by a constraint nobody wrote down.
	Items []OrderItem `gorm:"foreignKey:OrderID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (Order) TableName() string { return "orders" }

// OrderItem is one tier's line on an order: which tier, how many, and what each
// cost when it was bought.
//
// The tier link is RESTRICTed for the same reason the event link is — these
// rows are the record of what money was taken for. The title and unit price are
// COPIES rather than joins, so a renamed or re-priced tier cannot rewrite a
// receipt after the fact.
type OrderItem struct {
	ID string `gorm:"primaryKey;type:varchar(32)"`
	// OrderID and TicketID are unique together: one tier, one line. That index
	// is what makes a merged retry impossible to persist as two holds on the
	// same tier, whatever the caller sent — and it doubles as the lookup every
	// settlement uses to find an order's lines.
	OrderID     string `gorm:"not null;type:varchar(32);uniqueIndex:idx_order_items_order_ticket,priority:1"`
	TicketID    string `gorm:"not null;type:varchar(32);uniqueIndex:idx_order_items_order_ticket,priority:2;index:idx_order_items_ticket_id"`
	TicketTitle string `gorm:"not null;type:varchar(255);default:''"`

	Quantity       int   `gorm:"not null"`
	UnitPriceCents int64 `gorm:"not null;default:0"`
	TotalCents     int64 `gorm:"not null;default:0"`

	CreatedAt time.Time `gorm:"not null;autoCreateTime"`

	Ticket Ticket `gorm:"foreignKey:TicketID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

func (OrderItem) TableName() string { return "order_items" }
