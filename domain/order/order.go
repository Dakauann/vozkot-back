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
	"sort"
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
	ErrInvalidEvent        = errors.New("an order must name the event it is for")
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
	// ErrTooManyItems is an order spanning more tiers than one purchase may.
	ErrTooManyItems = errors.New("an order cannot span this many ticket tiers")
	// ErrMultipleEvents is a basket reaching across two nights. One order is
	// one event: the hold window, the door time and the venue on the receipt
	// all belong to a single one.
	ErrMultipleEvents = errors.New("an order cannot span more than one event")
)

// OpenHolds is what one buyer currently has reserved and unpaid.
//
// Two shapes because the abuse has two: a hundred small orders spread across an
// event, and one account sitting on an entire tier.
type OpenHolds struct {
	// Orders is how many pending_payment orders the buyer has open, across
	// every tier of every event.
	Orders int
	// TicketsByTier is how many tickets those open orders cover, per tier, for
	// the tiers being bought now. A tier the buyer holds nothing of is absent
	// rather than zero, which is the same thing to every reader of this map.
	//
	// A map and not a single number because one order may now span several
	// tiers, and a cap that only ever looked at the first of them would be a
	// cap in name only.
	TicketsByTier map[string]int
}

// HoldLimits caps what one account may keep off the shelf without paying.
//
// Without this, the per-minute rate limit does not close: at thirty checkouts a
// minute, ten tickets each and a thirty-minute hold, one account can keep nine
// thousand tickets unavailable indefinitely by cycling; the "hold an event
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

// Allows reports whether one more order covering `items` fits inside what the
// buyer is already holding.
//
// Every tier in the order is checked, and each check counts the new line too:
// the question is what the buyer would hold AFTER this checkout, not before it.
// Checking only one tier of a multi-tier order is how a per-tier cap gets
// multiplied by the number of tiers an event happens to have.
func (l HoldLimits) Allows(current OpenHolds, items []Item) error {
	if l.Orders > 0 && current.Orders >= l.Orders {
		return ErrTooManyOpenOrders
	}
	if l.TicketsPerTier <= 0 {
		return nil
	}
	for _, line := range items {
		if current.TicketsByTier[line.TicketID]+line.Quantity > l.TicketsPerTier {
			return ErrTooManyHeldTickets
		}
	}
	return nil
}

// MaxQuantityPerOrder caps a single purchase, counting every tier in it.
//
// Not an arbitrary number: an unbounded quantity lets one request reserve an
// entire event in a single call, which is both the classic scalper move and the
// easiest denial-of-service against a timed-hold system.
//
// It counts the ORDER, not the line. Capping each line instead would mean an
// event with three tiers had a real ceiling of thirty and one with ten had a
// hundred, a cap that loosens itself the more the organiser subdivides the
// house is not a cap.
const MaxQuantityPerOrder = 10

// MaxItemsPerOrder bounds how many distinct tiers one order may span.
//
// Independent of the quantity cap and needed on its own: ten lines of one
// ticket each obey MaxQuantityPerOrder while making the reservation loop ten
// row locks long, which is a cost a request should not get to choose freely.
const MaxItemsPerOrder = 10

// Item is one tier's share of an order: what was bought, how many, and what
// each cost at the moment of buying.
type Item struct {
	ID      string
	OrderID string
	// TicketID names the tier row whose stock this line reserves.
	TicketID string
	// TicketTitle is the tier's name AS IT WAS when the order was placed.
	//
	// A snapshot rather than a join, because a receipt must keep saying what
	// the buyer bought after the organiser renames "Pista" to "Pista Premium"
	// or deletes the tier outright. The tier is the authority for stock; this
	// is the authority for the record.
	TicketTitle string
	Quantity    int
	// UnitPriceCents is likewise the price AT PURCHASE. Re-reading it from the
	// tier would let a later price change rewrite what someone already paid.
	UnitPriceCents int64
	TotalCents     int64
}

// Order is one purchase against one event, covering one or more of its tiers.
//
// One EVENT and not one tier: a buyer choosing two Pista and one Camarote for
// the same night is making one purchase, and splitting that into two orders
// would give them two holds, two PIX codes and two chances to end up with half
// of what they wanted. One event and not several, because the hold window, the
// door time and the venue on the receipt all belong to a single night.
type Order struct {
	ID string
	// EventID is the night being bought. Every item belongs to it.
	EventID string
	// BuyerID is the authenticated account that placed the order, when there is
	// one. A box office also sells to people who never signed in.
	BuyerID       string
	BuyerName     string
	BuyerEmail    string
	BuyerDocument string

	// Items is never empty on a persisted order.
	Items      []Item
	TotalCents int64
	Currency   string

	Status Status
	// HoldExpiresAt is when the reserved stock goes back on sale. Meaningful
	// only while the order holds stock.
	HoldExpiresAt time.Time
	// Confirmed marks the order as having passed the buyer-details step, which
	// is what moves it off the short cart hold and onto the full payment
	// window. An unconfirmed order is a basket nobody has asked to pay for yet.
	Confirmed bool

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

// TotalQuantity is how many tickets the order covers across every tier.
func (o *Order) TotalQuantity() int {
	total := 0
	for _, line := range o.Items {
		total += line.Quantity
	}
	return total
}

// TicketIDs lists the tiers this order touches, in the order its items are
// held, which NormalizeItems has already sorted.
func (o *Order) TicketIDs() []string {
	ids := make([]string, 0, len(o.Items))
	for _, line := range o.Items {
		ids = append(ids, line.TicketID)
	}
	return ids
}

// DraftItem is one line a buyer asked for. The price is deliberately absent:
// it is read from the tier, so a client cannot name its own.
type DraftItem struct {
	TicketID string
	Quantity int
}

// Draft is what a buyer supplies.
type Draft struct {
	EventID        string
	BuyerID        string
	BuyerName      string
	BuyerEmail     string
	BuyerDocument  string
	Items          []Item
	Currency       string
	Method         payment.Method
	IdempotencyKey string
}

// NormalizeItems collapses repeated tiers, drops empty lines, and sorts.
//
// A client that names the same tier twice: two taps of "+" that raced, a retry
// that merged; means three of that tier, not two separate holds on it. Merging
// here rather than at the database keeps one tier to one row, which is what
// makes both the per-tier cap and the lock ordering below meaningful.
func NormalizeItems(items []DraftItem) ([]DraftItem, error) {
	merged := make([]DraftItem, 0, len(items))
	index := make(map[string]int, len(items))
	for _, line := range items {
		id := strings.TrimSpace(line.TicketID)
		if id == "" {
			return nil, ErrInvalidTicket
		}
		if line.Quantity <= 0 {
			// A zero line is a tier the buyer stepped back down to none of. Not
			// an error, simply not part of the order.
			continue
		}
		if at, seen := index[id]; seen {
			merged[at].Quantity += line.Quantity
			continue
		}
		index[id] = len(merged)
		merged = append(merged, DraftItem{TicketID: id, Quantity: line.Quantity})
	}
	if len(merged) == 0 {
		return nil, ErrInvalidQuantity
	}
	if len(merged) > MaxItemsPerOrder {
		return nil, ErrTooManyItems
	}
	total := 0
	for _, line := range merged {
		total += line.Quantity
	}
	if total > MaxQuantityPerOrder {
		return nil, ErrInvalidQuantity
	}
	// Sorted by tier id, and that is a correctness requirement rather than
	// tidiness. Reserving stock takes a row lock per tier; two orders covering
	// the same two tiers in opposite orders would each hold what the other
	// needs next, which is a deadlock PostgreSQL resolves by killing one of
	// them. Every order locking tiers in the same order makes it impossible.
	sort.Slice(merged, func(a, b int) bool { return merged[a].TicketID < merged[b].TicketID })
	return merged, nil
}

// New builds a pending order holding its items until holdFor elapses.
func New(id string, draft Draft, holdFor time.Duration, now time.Time) (*Order, error) {
	draft.BuyerName = strings.TrimSpace(draft.BuyerName)
	draft.BuyerEmail = strings.ToLower(strings.TrimSpace(draft.BuyerEmail))
	draft.BuyerDocument = onlyDigits(draft.BuyerDocument)

	if strings.TrimSpace(draft.EventID) == "" {
		return nil, ErrInvalidEvent
	}
	if len(draft.Items) == 0 {
		return nil, ErrInvalidQuantity
	}
	if len(draft.Items) > MaxItemsPerOrder {
		return nil, ErrTooManyItems
	}

	total := int64(0)
	quantity := 0
	for index := range draft.Items {
		line := &draft.Items[index]
		line.TicketID = strings.TrimSpace(line.TicketID)
		if line.TicketID == "" {
			return nil, ErrInvalidTicket
		}
		if line.Quantity < 1 {
			return nil, ErrInvalidQuantity
		}
		if line.UnitPriceCents < 0 {
			return nil, payment.ErrInvalidAmount
		}
		line.TotalCents = line.UnitPriceCents * int64(line.Quantity)
		total += line.TotalCents
		quantity += line.Quantity
	}
	if quantity > MaxQuantityPerOrder {
		return nil, ErrInvalidQuantity
	}

	// The buyer's name and email are checked only when they are supplied. A
	// cart hold is opened before the form is filled in, and refusing it for a
	// blank name would mean holding nothing until the buyer finished typing,
	// which is the race the early hold exists to remove. Confirm is where the
	// same two fields become mandatory.
	if draft.BuyerName != "" || draft.BuyerEmail != "" {
		if err := validateBuyer(draft.BuyerName, draft.BuyerEmail); err != nil {
			return nil, err
		}
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
		ID:            id,
		EventID:       strings.TrimSpace(draft.EventID),
		BuyerID:       strings.TrimSpace(draft.BuyerID),
		BuyerName:     draft.BuyerName,
		BuyerEmail:    draft.BuyerEmail,
		BuyerDocument: draft.BuyerDocument,
		Items:         draft.Items,
		TotalCents:    total,
		Currency:      currency,
		Status:        StatusPendingPayment,
		HoldExpiresAt: timestamp.Add(holdFor),
		// No provider named here, deliberately. An order that has not been
		// charged yet has not been through ANY provider, and stamping one at
		// creation was how every order ended up labelled "mercadopago" whatever
		// actually issued the charge. AttachCharge sets it from the charge, so
		// the field says what really happened rather than what was configured
		// when the row was written.
		PaymentStatus:  payment.StatusPending,
		PaymentMethod:  method,
		IdempotencyKey: strings.TrimSpace(draft.IdempotencyKey),
		CreatedAt:      timestamp,
		UpdatedAt:      timestamp,
	}, nil
}

// Confirm records the buyer's details and moves the order off the short cart
// hold and onto the full payment window.
//
// The extension happens ONCE, however many times this is called. A buyer who
// submits the form twice, or whose phone retried the request, must not get a
// second window; an extension per press is a way to hold stock forever by
// pressing a button.
func (o *Order) Confirm(
	name, email, document string,
	method payment.Method,
	holdFor time.Duration,
	now time.Time,
) (bool, error) {
	if !o.Status.HoldsStock() {
		// Nothing to confirm: the hold is gone, expired, cancelled or paid.
		return false, ErrHoldExpired
	}
	name = strings.TrimSpace(name)
	email = strings.ToLower(strings.TrimSpace(email))
	if err := validateBuyer(name, email); err != nil {
		return false, err
	}

	timestamp := now.UTC()
	o.BuyerName = name
	o.BuyerEmail = email
	if digits := onlyDigits(document); digits != "" {
		o.BuyerDocument = digits
	}
	if method != "" {
		o.PaymentMethod = method
	}
	o.UpdatedAt = timestamp

	if o.Confirmed {
		// Details corrected on a second pass. Worth saving; not worth a second
		// window.
		return false, nil
	}
	o.Confirmed = true
	// Only ever forward. A hold already further out than the new window, an
	// operator-lengthened one, or a clock that disagrees; must not be pulled
	// backwards by a confirmation.
	if extended := timestamp.Add(holdFor); extended.After(o.HoldExpiresAt) {
		o.HoldExpiresAt = extended
	}
	return true, nil
}

func validateBuyer(name, email string) error {
	if name == "" {
		return ErrInvalidBuyerName
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return ErrInvalidBuyerEmail
	}
	return nil
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
