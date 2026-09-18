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

	// The audience snapshot, frozen at purchase and stored in the CLEAR.
	//
	// The buyer's own copy of these is sealed on the users table; this one is
	// not, and the difference is deliberate. A gender, a whole number of years,
	// a city and a state identify nobody on their own, and this is the only
	// form of them anything ever aggregates: the organiser's report is a GROUP
	// BY over these columns, and an encrypted column would make that report a
	// full decrypt of every order ever placed. Sealing here would buy no
	// privacy and cost the feature.
	//
	// Every one is optional. An empty string and a zero age mean "not
	// informed", which every breakdown counts as its own row.
	BuyerGender   string `gorm:"not null;type:varchar(16);default:''"`
	BuyerAgeYears int    `gorm:"not null;default:0"`
	BuyerCity     string `gorm:"not null;type:varchar(120);default:''"`
	BuyerUF       string `gorm:"not null;type:varchar(2);default:''"`

	// SubtotalCents is the organiser's share and ServiceFeeCents is ours;
	// TotalCents is what the buyer paid and is always the sum of the two.
	//
	// Three columns rather than two plus arithmetic, because the fee RATE that
	// produced them is not stored anywhere on this row and must not have to be:
	// recomputing an old order's split from today's rate is how a payout report
	// starts disagreeing with a receipt.
	SubtotalCents   int64  `gorm:"not null;default:0"`
	ServiceFeeCents int64  `gorm:"not null;default:0"`
	TotalCents      int64  `gorm:"not null;default:0"`
	Currency        string `gorm:"not null;type:varchar(3);default:'BRL'"`
	// RefundPolicyVersion is the cancellation rules frozen at checkout. Orders
	// written before refunds existed carry 0 and are read as the oldest known
	// policy; see usecases/refund.
	RefundPolicyVersion int `gorm:"not null;default:0"`

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
	// create the foreign key immediately, against a table whose existing rows
	// have not been backfilled yet, which fails every boot that has orders
	// predating the column. The constraint is added by enforceOrderEventLink
	// once every order has an event, and skipped while any does not.

	// The items constraint is declared HERE, on the has-many side, and not on a
	// belongs-to field over on OrderItem. Declaring it in both places makes
	// GORM emit two foreign keys over the same column, and the one it derives
	// from this side carries no ON DELETE, so deleting an order would be
	// refused by a constraint nobody wrote down.
	Items []OrderItem `gorm:"foreignKey:OrderID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (Order) TableName() string { return "orders" }

// OrderItem is one tier's line on an order: which tier, how many, and what each
// cost when it was bought.
//
// The tier link is RESTRICTed for the same reason the event link is, these
// rows are the record of what money was taken for. The title and unit price are
// COPIES rather than joins, so a renamed or re-priced tier cannot rewrite a
// receipt after the fact.
type OrderItem struct {
	ID string `gorm:"primaryKey;type:varchar(32)"`
	// OrderID and TicketID were unique together: one tier, one line. That
	// still holds for a COUNTED line and it is what makes a merged retry
	// impossible to persist as two holds on the same tier.
	//
	// It stopped being expressible as a tag when reserved seating arrived. A
	// seated line is one row per CHAIR, so four seats of one tier are four rows
	// sharing an order and a tier, and this index would reject a legitimate
	// purchase. It is replaced by two PARTIAL unique indexes in indexes.go:
	// one per tier for counted lines, one per seat for seated ones, because
	// widening it to include seat_id would have quietly dropped the counted
	// guarantee: PostgreSQL treats NULLs as distinct, so two counted lines for
	// the same tier would both be accepted.
	//
	// The plain index that remains is the lookup every settlement uses to find
	// an order's lines.
	OrderID     string `gorm:"not null;type:varchar(32);index:idx_order_items_order_ticket,priority:1"`
	TicketID    string `gorm:"not null;type:varchar(32);index:idx_order_items_order_ticket,priority:2;index:idx_order_items_ticket_id"`
	TicketTitle string `gorm:"not null;type:varchar(255);default:''"`

	Quantity int `gorm:"not null"`
	// UnitPriceCents and TotalCents are the FACE value: the organiser's price
	// and their share of this line. The fee columns are the commission on top,
	// per unit and for the line, so what the buyer paid for this line is
	// TotalCents + FeeCents.
	UnitPriceCents int64 `gorm:"not null;default:0"`
	TotalCents     int64 `gorm:"not null;default:0"`
	UnitFeeCents   int64 `gorm:"not null;default:0"`
	// SeatID names the reserved seat this line bought, NULL for a counted line.
	//
	// Nullable rather than empty-string because the partial unique indexes in
	// indexes.go key off IS NULL / IS NOT NULL to tell the two kinds of line
	// apart, and an empty string would make every counted line collide.
	//
	// No foreign key to event_seats. The seat columns beside it are a SNAPSHOT,
	// and the whole point of a snapshot is that it survives the row it was
	// taken from; a RESTRICT here would block an organiser from ever retiring a
	// layout, and a CASCADE would delete the record of a sale.
	SeatID      *string `gorm:"type:varchar(32);index:idx_order_items_seat_id"`
	SeatSection string  `gorm:"not null;type:varchar(120);default:''"`
	SeatRow     string  `gorm:"not null;type:varchar(16);default:''"`
	SeatLabel   string  `gorm:"not null;type:varchar(16);default:''"`
	SeatKind    string  `gorm:"not null;type:varchar(24);default:''"`
	FeeCents    int64   `gorm:"not null;default:0"`

	CreatedAt time.Time `gorm:"not null;autoCreateTime"`

	Ticket Ticket `gorm:"foreignKey:TicketID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

func (OrderItem) TableName() string { return "order_items" }
