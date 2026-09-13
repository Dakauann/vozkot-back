// Package order is a purchase of ingressos: the buyer, the quantity, the money
// owed, and the hold that keeps those tickets off the shelf while they pay.
//
// The invariant this package exists to protect is that stock and orders never
// disagree. Every transition below is paired with a stock movement in the use
// case, and every transition is idempotent, because the events that drive them
// arrive from a payment provider that redelivers.
package order

import (
	"errors"
	"net/mail"
	"strings"
	"time"

	"vozkot/domain/payment"
)

type Status string

const (
	// StatusPendingPayment holds stock. It is the only status that does.
	StatusPendingPayment Status = "pending_payment"
	// StatusPaid converted the hold into a sale.
	StatusPaid Status = "paid"
	// StatusExpired ran out the hold window without payment.
	StatusExpired Status = "expired"
	// StatusCancelled was given up by the buyer or the operator.
	StatusCancelled Status = "cancelled"
	// StatusFailed was rejected by the payment provider.
	StatusFailed Status = "failed"
	// StatusRefunded was paid and given back; the tickets returned to stock.
	StatusRefunded Status = "refunded"
	// StatusRefundRequired is money taken for tickets that no longer exist: a
	// payment that arrived after the hold expired and the stock was resold. It
	// is a real outcome of any timed-hold system and the honest thing to do is
	// name it and owe the buyer a refund, not to quietly mark the order paid and
	// oversell the event.
	StatusRefundRequired Status = "refund_required"
)

// holdsStock is the single source of truth for "does this order own inventory".
func (s Status) HoldsStock() bool { return s == StatusPendingPayment }

// Final reports whether an order has reached a resting state.
func (s Status) Final() bool {
	switch s {
	case StatusPaid, StatusExpired, StatusCancelled, StatusFailed, StatusRefunded, StatusRefundRequired:
		return true
	default:
		return false
	}
}

func (s Status) Valid() bool {
	switch s {
	case StatusPendingPayment, StatusPaid, StatusExpired, StatusCancelled,
		StatusFailed, StatusRefunded, StatusRefundRequired:
		return true
	default:
		return false
	}
}

var (
	ErrNotFound            = errors.New("order not found")
	ErrInvalidTicket       = errors.New("ticket is required")
	ErrInvalidQuantity     = errors.New("quantity must be between 1 and the per-order limit")
	ErrInvalidBuyerName    = errors.New("buyer name is required")
	ErrInvalidBuyerEmail   = errors.New("buyer email is invalid")
	ErrInvalidTransition   = errors.New("order status transition is not allowed")
	ErrAlreadyFinal        = errors.New("order has already reached a final status")
	ErrHoldExpired         = errors.New("order hold has expired")
	ErrIdempotencyMismatch = errors.New("idempotency key was reused with a different request")
	// ErrTooManyOpenOrders is one account sitting on more unpaid orders than the
	// box office allows. The buyer pays or cancels one before opening another.
	ErrTooManyOpenOrders = errors.New("this account already has the maximum number of orders awaiting payment")
	// ErrTooManyHeldTickets is one account holding more of a single tier than
	// the box office allows.
	ErrTooManyHeldTickets = errors.New("this account already holds the maximum number of tickets for this tier")
)

// OpenHolds is what one buyer currently has reserved and unpaid.
//
// Two numbers because the abuse has two shapes: a hundred small orders spread
// across an event, and one account sitting on an entire tier.
type OpenHolds struct {
	// Orders is how many pending_payment orders the buyer has open, across
	// every tier of every event.
	Orders int
	// TicketsForTier is how many tickets those open orders cover on the ONE
	// tier being bought now.
	TicketsForTier int
}

// HoldLimits caps what one account may keep off the shelf without paying.
//
// Without this, the per-minute rate limit does not close: at thirty checkouts a
// minute, ten tickets each and a thirty-minute hold, one account can keep nine
// thousand tickets unavailable indefinitely by cycling — the "hold an event
// hostage" move, executed with no money at risk. The rate limit bounds how FAST
// someone reserves; this bounds how MUCH they may be sitting on at once, which
// is the quantity that actually hurts.
//
// A zero or negative value means that dimension is not capped, so an operator
// can disable either one without a code change.
type HoldLimits struct {
	// Orders is the most pending_payment orders one account may have open.
	Orders int
	// TicketsPerTier is the most tickets of one tier one account may hold
	// across all of those orders.
	TicketsPerTier int
}

// Unlimited reports whether these limits cap nothing, so a caller can skip the
// lock and the count entirely.
func (l HoldLimits) Unlimited() bool { return l.Orders <= 0 && l.TicketsPerTier <= 0 }

// Allows reports whether one more order for `quantity` tickets fits inside what
// the buyer is already holding.
//
// The tier check counts the new order too: the question is what the buyer would
// hold AFTER this checkout, not before it.
func (l HoldLimits) Allows(current OpenHolds, quantity int) error {
	if l.Orders > 0 && current.Orders >= l.Orders {
		return ErrTooManyOpenOrders
	}
	if l.TicketsPerTier > 0 && current.TicketsForTier+quantity > l.TicketsPerTier {
		return ErrTooManyHeldTickets
	}
	return nil
}

// MaxQuantityPerOrder caps a single purchase.
//
// Not an arbitrary number: an unbounded quantity lets one request reserve an
// entire event in a single call, which is both the classic scalper move and the
// easiest denial-of-service against a timed-hold system.
const MaxQuantityPerOrder = 10

// Order is one purchase attempt against one ticket tier.
type Order struct {
	ID       string
	TicketID string
	// BuyerID is the authenticated account that placed the order, when there is
	// one. A box office also sells to people who never signed in.
	BuyerID       string
	BuyerName     string
	BuyerEmail    string
	BuyerDocument string

	Quantity       int
	UnitPriceCents int64
	TotalCents     int64
	Currency       string

	Status Status
	// HoldExpiresAt is when the reserved stock goes back on sale. Meaningful
	// only while the order holds stock.
	HoldExpiresAt time.Time

	PaymentProvider payment.Provider
	PaymentID       string
	PaymentStatus   payment.Status
	PaymentMethod   payment.Method
	PixCopyPaste    string
	PixQRCodeBase64 string

	// IdempotencyKey is the client's key for the request that created this
	// order, kept so a replay can be tied back to the order it produced.
	IdempotencyKey string

	PaidAt    *time.Time
	ClosedAt  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Draft is what a buyer supplies.
type Draft struct {
	TicketID       string
	BuyerID        string
	BuyerName      string
	BuyerEmail     string
	BuyerDocument  string
	Quantity       int
	UnitPriceCents int64
	Currency       string
	Method         payment.Method
	IdempotencyKey string
}

// New builds a pending order holding `Quantity` tickets until holdFor elapses.
func New(id string, draft Draft, holdFor time.Duration, now time.Time) (*Order, error) {
	draft.BuyerName = strings.TrimSpace(draft.BuyerName)
	draft.BuyerEmail = strings.ToLower(strings.TrimSpace(draft.BuyerEmail))
	draft.BuyerDocument = onlyDigits(draft.BuyerDocument)

	if strings.TrimSpace(draft.TicketID) == "" {
		return nil, ErrInvalidTicket
	}
	if draft.Quantity < 1 || draft.Quantity > MaxQuantityPerOrder {
		return nil, ErrInvalidQuantity
	}
	if draft.BuyerName == "" {
		return nil, ErrInvalidBuyerName
	}
	if _, err := mail.ParseAddress(draft.BuyerEmail); err != nil {
		return nil, ErrInvalidBuyerEmail
	}
	if draft.UnitPriceCents < 0 {
		return nil, payment.ErrInvalidAmount
	}

	method := draft.Method
	if method == "" {
		method = payment.MethodPix
	}
	currency := strings.TrimSpace(draft.Currency)
	if currency == "" {
		currency = "BRL"
	}

	timestamp := now.UTC()
	return &Order{
		ID:              id,
		TicketID:        strings.TrimSpace(draft.TicketID),
		BuyerID:         strings.TrimSpace(draft.BuyerID),
		BuyerName:       draft.BuyerName,
		BuyerEmail:      draft.BuyerEmail,
		BuyerDocument:   draft.BuyerDocument,
		Quantity:        draft.Quantity,
		UnitPriceCents:  draft.UnitPriceCents,
		TotalCents:      draft.UnitPriceCents * int64(draft.Quantity),
		Currency:        currency,
		Status:          StatusPendingPayment,
		HoldExpiresAt:   timestamp.Add(holdFor),
		PaymentProvider: payment.ProviderMercadoPago,
		PaymentStatus:   payment.StatusPending,
		PaymentMethod:   method,
		IdempotencyKey:  strings.TrimSpace(draft.IdempotencyKey),
		CreatedAt:       timestamp,
		UpdatedAt:       timestamp,
	}, nil
}

// transitions is the whole state machine, written once.
//
// Re-applying the status an order already has is NOT in here: it is handled
// before the table is consulted and reported as "nothing changed", because a
// payment provider redelivering the same approval must be a no-op and not an
// error the queue then retries forever.
var transitions = map[Status][]Status{
	StatusPendingPayment: {StatusPaid, StatusExpired, StatusCancelled, StatusFailed, StatusRefundRequired},
	// A hold that lapsed can still be settled if the stock is re-reserved; the
	// use case decides which of the two it is, and this table permits both.
	StatusExpired:   {StatusPaid, StatusRefundRequired},
	StatusCancelled: {StatusPaid, StatusRefundRequired},
	StatusFailed:    {StatusPaid, StatusRefundRequired},
	StatusPaid:      {StatusRefunded},
	// Terminal.
	StatusRefunded:       {},
	StatusRefundRequired: {StatusRefunded},
}

// CanTransition reports whether next is reachable from the current status.
func (o *Order) CanTransition(next Status) bool {
	if !next.Valid() {
		return false
	}
	for _, allowed := range transitions[o.Status] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Apply moves the order to next.
//
// The bool answers "did this change anything", which is what makes the whole
// pipeline safe to run twice: a redelivered webhook applies the same status,
// gets false, and the caller skips the stock movement instead of committing it
// a second time.
func (o *Order) Apply(next Status, now time.Time) (bool, error) {
	if !next.Valid() {
		return false, ErrInvalidTransition
	}
	if o.Status == next {
		return false, nil
	}
	if !o.CanTransition(next) {
		return false, ErrInvalidTransition
	}

	timestamp := now.UTC()
	o.Status = next
	o.UpdatedAt = timestamp
	switch next {
	case StatusPaid:
		if o.PaidAt == nil {
			o.PaidAt = &timestamp
		}
	case StatusExpired, StatusCancelled, StatusFailed, StatusRefunded, StatusRefundRequired:
		if o.ClosedAt == nil {
			o.ClosedAt = &timestamp
		}
	}
	return true, nil
}

// AttachCharge records what the provider issued. Kept separate from Apply
// because a charge arriving does not by itself move the order: a created PIX
// charge is still an unpaid order.
func (o *Order) AttachCharge(charge *payment.Charge, now time.Time) {
	if charge == nil {
		return
	}
	o.PaymentProvider = charge.Provider
	o.PaymentID = charge.ID
	o.PaymentStatus = charge.Status
	if charge.Method != "" {
		o.PaymentMethod = charge.Method
	}
	if charge.PixCopyPaste != "" {
		o.PixCopyPaste = charge.PixCopyPaste
	}
	if charge.PixQRCodeBase64 != "" {
		o.PixQRCodeBase64 = charge.PixQRCodeBase64
	}
	o.UpdatedAt = now.UTC()
}

// HoldLapsed reports whether the reservation window has passed.
func (o *Order) HoldLapsed(now time.Time) bool {
	return o.Status.HoldsStock() && !now.UTC().Before(o.HoldExpiresAt)
}

// StatusFor maps a charge state onto the order state it implies.
//
// One place, so the webhook path, the reconciliation sweep and any manual
// replay can never disagree about what "approved" means.
func StatusFor(chargeStatus payment.Status) (Status, bool) {
	switch chargeStatus {
	case payment.StatusPaid:
		return StatusPaid, true
	case payment.StatusRejected:
		return StatusFailed, true
	case payment.StatusCancelled:
		return StatusExpired, true
	case payment.StatusRefunded, payment.StatusChargedBack:
		return StatusRefunded, true
	default:
		// Pending and in-analysis deliberately move nothing.
		return "", false
	}
}

func onlyDigits(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if char >= '0' && char <= '9' {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}
