package schema

import "time"

// RefundRequest is one refund in flight, with the record of who asked and who
// agreed.
//
// Its own table rather than columns on the order, because a REJECTED request
// leaves no trace on the order at all and is exactly the case support is asked
// about later. The order says what happened to the money; this says what
// happened to the conversation.
//
// The order reference is RESTRICTed, like every other row that is the record of
// money changing hands: deleting an order that somebody asked a refund for
// would delete the evidence of the asking.
type RefundRequest struct {
	ID string `gorm:"primaryKey;type:varchar(32)"`
	// OrderID carries no unique index here. Uniqueness is PARTIAL: one OPEN
	// request per order, any number of closed ones, and a partial index cannot
	// be expressed in a struct tag, so it is built in indexes.go. A total unique
	// index would let one rejected request block every future one.
	OrderID string `gorm:"not null;type:varchar(32);index:idx_refund_requests_order_id"`
	// EventID is copied from the order so an organiser's inbox is one indexed
	// read rather than a join through orders on every page load.
	EventID string `gorm:"not null;type:varchar(32);default:'';index:idx_refund_requests_event_id"`
	BuyerID string `gorm:"not null;type:varchar(32);default:'';index:idx_refund_requests_buyer_id"`

	Reason string `gorm:"not null;type:varchar(32)"`
	Status string `gorm:"not null;type:varchar(16);index:idx_refund_requests_status"`

	// AmountCents is what the buyer gets back and FeeCents is our share of it,
	// both frozen when the request was opened. Frozen so that a later change to
	// the order cannot silently change what somebody approved.
	AmountCents int64 `gorm:"not null;default:0"`
	FeeCents    int64 `gorm:"not null;default:0"`

	RequestedBy string `gorm:"not null;type:varchar(32);default:''"`
	Note        string `gorm:"type:text;not null;default:''"`
	// DecidedBy is empty when the POLICY approved it rather than a person. That
	// emptiness is the audit trail's way of recording "no human was involved",
	// which is why it is not defaulted to the requester.
	DecidedBy    string `gorm:"not null;type:varchar(32);default:''"`
	DecisionNote string `gorm:"type:text;not null;default:''"`
	DecidedAt    *time.Time

	CreatedAt time.Time `gorm:"not null;autoCreateTime;index:idx_refund_requests_created_at"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime"`

	Order Order `gorm:"foreignKey:OrderID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

func (RefundRequest) TableName() string { return "refund_requests" }
