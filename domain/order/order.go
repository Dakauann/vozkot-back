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
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"vozkot/domain/payment"
	"vozkot/domain/pricing"
	"vozkot/domain/seating"
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
	// ErrSeatCountMismatch is a line that named a quantity and a different
	// number of seats. Either number could be the one the buyer meant, so
	// neither is used.
	ErrSeatCountMismatch = errors.New("a seated line's quantity must match the seats it names")
	// ErrMixedSeating is one tier asked for both ways in one basket. A tier is
	// either seated or counted, never both, so this is a client bug rather than
	// a buyer's choice.
	ErrMixedSeating = errors.New("a tier cannot be bought as seats and as quantity in one order")
	// ErrMultipleEvents is a basket reaching across two nights. One order is
	// one event: the hold window, the door time and the venue on the receipt
	// all belong to a single one.
	ErrMultipleEvents = errors.New("an order cannot span more than one event")
	// ErrNoRefundPolicy is an order created without cancellation rules frozen
	// onto it. Refused rather than defaulted: a default here would be a policy
	// nobody chose, applied to somebody's money.
	ErrNoRefundPolicy = errors.New("an order must carry the refund policy it was bought under")
	// ErrPricingInconsistent is an order whose three money columns disagree.
	//
	// It cannot happen on an order New built, because New derives all three in
	// one pass. It can happen to an order that came back from storage: a
	// half-applied backfill, a mixed-version deploy writing lines one schema
	// knew about and another did not, or somebody correcting a total by hand
	// in psql. Charging such a row would bill the buyer one number and credit
	// the organiser from another, and the difference is only ever found by
	// reconciliation weeks later.
	ErrPricingInconsistent = errors.New("order pricing does not add up")
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
	//
	// It is the FACE value: what the organiser asked for one ticket, and what
	// they are owed for it. The box office's commission is the field below and
	// is never mixed into this one, because the moment they are added together
	// there is no way to answer "what do we owe the organiser" without knowing
	// the fee rate that was live months ago.
	UnitPriceCents int64
	// TotalCents is UnitPriceCents × Quantity: the organiser's share of this
	// line, still excluding the fee.
	TotalCents int64
	// UnitFeeCents is the service fee on ONE ticket of this tier, and FeeCents
	// is that times the quantity — never a fee taken on the line total, so an
	// event page that quotes one ticket has told the truth about three.
	//
	// Both are snapshots for the same reason the price is: a rate change must
	// not rewrite what somebody was charged.
	UnitFeeCents int64
	FeeCents     int64

	// --- the seat, when there is one ----------------------------------------
	//
	// SeatID names the reserved seat this line bought. Empty for a counted
	// line, which is every line of a general-admission event.
	//
	// A seated line always has Quantity 1: one row per chair. That is a
	// deliberate asymmetry with counted lines, and it is what makes refunding
	// one seat out of four, and issuing one admission per chair, fall out
	// instead of needing a second model.
	SeatID string
	// Seat is the label AS IT WAS, the same snapshot rule TicketTitle follows.
	// The venue re-lettering row I next season must not rewrite this ticket.
	//
	// seating.Label rather than three strings of our own: the door, the wallet,
	// the receipt and the confirmation email all have to say the same thing
	// about a chair somebody is standing in front of, and four copies of that
	// format is four chances to disagree.
	Seat     seating.Label
	SeatKind seating.SeatKind
}

// Seated reports whether this line bought a named seat.
func (i Item) Seated() bool { return strings.TrimSpace(i.SeatID) != "" }

// ChargedCents is what the buyer paid for this line: face plus fee.
func (i Item) ChargedCents() int64 { return i.TotalCents + i.FeeCents }

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

	// --- what the buyer told us about themselves, frozen at purchase --------
	//
	// A COPY, exactly like the tier title and the unit price above, and for the
	// same reason: a buyer who moves from Recife to Lisbon next year must not
	// retroactively move last year's audience, and one who fills in their
	// gender for the first time must not change a report that was already run.
	// The report is about who bought the ticket THEN.
	//
	// It is also the only form of this data an organiser ever sees aggregated,
	// and it is deliberately coarse: a gender, a whole number of years, a city
	// and a state. Nothing here identifies anybody, which is what makes it safe
	// to GROUP BY in the clear while the buyer's own profile stays sealed.
	// Every field is optional and an empty one means "not informed", counted as
	// its own row in every breakdown rather than silently dropped.
	BuyerGender string
	// BuyerAgeYears is whole years at the moment of purchase, 0 when unknown.
	// An age rather than a date of birth on purpose: the date is identifying
	// and belongs in the sealed profile, the age is the fact the report needs.
	BuyerAgeYears int
	BuyerCity     string
	BuyerUF       string

	// Items is never empty on a persisted order.
	Items []Item
	// SubtotalCents is the sum of the lines' face values: the organiser's share
	// of this order.
	SubtotalCents int64
	// BuyerFeeCents is the service fee added ON TOP, which is ours.
	BuyerFeeCents int64
	// TotalCents is what the buyer pays and what is charged: subtotal + fee.
	//
	// It keeps that meaning from before the fee existed, which is why the
	// charge, the refund and every receipt still read this field and did not
	// have to change: the number they wanted was always "what the buyer owes".
	TotalCents int64
	Currency   string

	// RefundPolicyVersion is the cancellation rules frozen at checkout. An
	// organiser tightening the policy on Tuesday must not have tightened it for
	// somebody who bought on Monday; see domain/refund.
	RefundPolicyVersion int

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

// ValidatePricing re-checks the split on an order that came from storage.
//
// New guarantees this at construction; this method is for every later read.
// It is deliberately a separate check rather than something the repository
// does on load: the cost belongs on the path that is about to move money, not
// on every listing query.
//
// Three things have to hold, and each catches a different accident:
//
//   - the lines' face values sum to the subtotal, and their fees to the fee, so
//     a line written by a different schema version cannot hide inside a total
//     that still looks plausible;
//   - the total is exactly subtotal + fee, so the number the buyer is charged
//     is the number the organiser's payout was computed against;
//   - nothing is negative, because a negative fee is a refund wearing a
//     charge's clothes.
func (o *Order) ValidatePricing() error {
	if o.SubtotalCents < 0 || o.BuyerFeeCents < 0 || o.TotalCents < 0 {
		return fmt.Errorf("%w: negative money on order %s (subtotal %d, fee %d, total %d)",
			ErrPricingInconsistent, o.ID, o.SubtotalCents, o.BuyerFeeCents, o.TotalCents)
	}
	if o.TotalCents != o.SubtotalCents+o.BuyerFeeCents {
		return fmt.Errorf("%w: order %s totals %d but its subtotal %d and fee %d sum to %d",
			ErrPricingInconsistent, o.ID, o.TotalCents,
			o.SubtotalCents, o.BuyerFeeCents, o.SubtotalCents+o.BuyerFeeCents)
	}

	// An order with no lines is not a pricing fault: a hold can be read back
	// without them by a query that did not ask for them, and the totals above
	// are still the authority on what is owed.
	if len(o.Items) == 0 {
		return nil
	}

	face := int64(0)
	fee := int64(0)
	for _, item := range o.Items {
		face += item.TotalCents
		fee += item.FeeCents
	}
	if face != o.SubtotalCents || fee != o.BuyerFeeCents {
		return fmt.Errorf("%w: order %s lines sum to subtotal %d and fee %d, but the order says %d and %d",
			ErrPricingInconsistent, o.ID, face, fee, o.SubtotalCents, o.BuyerFeeCents)
	}
	return nil
}

// Reference is the order id a person reads out: short, uppercase and without
// the internal prefix, but still enough of the id to find the row.
//
// One definition, because more than one surface shows it and they have to
// agree. It was briefly written twice — the receipt took the first eight
// characters of the id and the door took the last eight — which would have had
// a buyer reading a reference off their email that the doorperson could not
// find.
func Reference(orderID string) string {
	trimmed := strings.TrimPrefix(orderID, "ord_")
	if len(trimmed) > ReferenceLength {
		trimmed = trimmed[:ReferenceLength]
	}
	return strings.ToUpper(trimmed)
}

// ReferenceLength is how much of the id a reference keeps.
const ReferenceLength = 8

// Reference is this order's short form.
func (o *Order) Reference() string { return Reference(o.ID) }

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
	// SeatIDs names the reserved seats this line is for, and is empty for a
	// counted line.
	//
	// Its presence is what makes a line SEATED. Quantity is then derived from
	// it rather than trusted: a line that says three and names four chairs is a
	// client bug, and picking either number would sell somebody the wrong thing.
	SeatIDs []string
}

// Seated reports whether this line buys named seats rather than counted stock.
func (d DraftItem) Seated() bool { return len(d.SeatIDs) > 0 }

// Draft is what a buyer supplies, plus what the box office adds to it.
type Draft struct {
	EventID       string
	BuyerID       string
	BuyerName     string
	BuyerEmail    string
	BuyerDocument string
	// The optional demographics, read from the buyer's profile by the use case
	// rather than sent by the client: a report an unauthenticated body could
	// write into is a report that says whatever somebody wanted it to say.
	BuyerGender   string
	BuyerAgeYears int
	BuyerCity     string
	BuyerUF       string

	Items []Item
	// Fee is the box office's commission, applied here so that exactly one
	// place in the system turns a tier price into a charge.
	Fee            pricing.Fee
	Currency       string
	Method         payment.Method
	IdempotencyKey string
	// RefundPolicyVersion is frozen onto the order. Zero is rejected: an order
	// with no cancellation rules is an order nobody can answer a question about.
	RefundPolicyVersion int
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
		// Seats first, because for a seated line they decide the quantity.
		// seating.NormalizeSeatIDs trims, de-duplicates and sorts; the sort is
		// the same deadlock defence this function applies to tiers below, one
		// level down.
		seats := seating.NormalizeSeatIDs(line.SeatIDs)
		if len(seats) > 0 {
			// A quantity sent alongside seats has to agree with them. Deriving
			// silently would hide a client that lost a seat between the picker
			// and the request; refusing says so while nothing has been held.
			if line.Quantity > 0 && line.Quantity != len(seats) {
				return nil, ErrSeatCountMismatch
			}
			line.Quantity = len(seats)
		}
		if line.Quantity <= 0 {
			// A zero line is a tier the buyer stepped back down to none of. Not
			// an error, simply not part of the order.
			continue
		}
		if at, seen := index[id]; seen {
			// One tier, two lines. Merging counted quantities is what this
			// always did; merging seats is the union of the two sets, and
			// mixing the two forms is a contradiction rather than a sum.
			if merged[at].Seated() != (len(seats) > 0) {
				return nil, ErrMixedSeating
			}
			if len(seats) > 0 {
				merged[at].SeatIDs = seating.NormalizeSeatIDs(append(merged[at].SeatIDs, seats...))
				merged[at].Quantity = len(merged[at].SeatIDs)
				continue
			}
			merged[at].Quantity += line.Quantity
			continue
		}
		index[id] = len(merged)
		merged = append(merged, DraftItem{TicketID: id, Quantity: line.Quantity, SeatIDs: seats})
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

// distinctTiers counts how many tiers a set of lines spans.
func distinctTiers(items []Item) int {
	seen := make(map[string]struct{}, len(items))
	for _, line := range items {
		seen[strings.TrimSpace(line.TicketID)] = struct{}{}
	}
	return len(seen)
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
	// DISTINCT TIERS, not rows, which is what the constant has always meant
	// and no longer the same number. A seated line is one row per chair, so
	// four seats of Plateia Premium are four rows spanning ONE tier, and
	// counting rows here would refuse a legitimate purchase of five.
	//
	// It happens that MaxQuantityPerOrder caps the total at ten anyway, so
	// counting rows would not reject anything today. That is arithmetic
	// coincidence between two independent constants, and the next person to
	// raise one of them should not have to discover this.
	if distinctTiers(draft.Items) > MaxItemsPerOrder {
		return nil, ErrTooManyItems
	}

	// Priced HERE and only here. The tier supplies a face value, the fee
	// supplies a rate, and the three totals below are derived from them in one
	// pass so that no caller can arrive at a different answer: the charge, the
	// receipt, the refund and the organiser's payout all read these fields.
	subtotal := int64(0)
	fees := int64(0)
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
		priced := draft.Fee.Quote(line.UnitPriceCents, line.Quantity)
		line.TotalCents = priced.FaceCents
		line.UnitFeeCents = priced.UnitFeeCents
		line.FeeCents = priced.FeeCents
		subtotal += priced.FaceCents
		fees += priced.FeeCents
		quantity += line.Quantity
	}
	if quantity > MaxQuantityPerOrder {
		return nil, ErrInvalidQuantity
	}
	if draft.RefundPolicyVersion <= 0 {
		return nil, ErrNoRefundPolicy
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
		BuyerGender:   strings.TrimSpace(draft.BuyerGender),
		BuyerAgeYears: draft.BuyerAgeYears,
		BuyerCity:     strings.TrimSpace(draft.BuyerCity),
		BuyerUF:       strings.ToUpper(strings.TrimSpace(draft.BuyerUF)),
		Items:         draft.Items,
		SubtotalCents: subtotal,
		BuyerFeeCents: fees,
		// What the buyer owes, which is what gets charged. Kept as the sum of
		// the two rather than recomputed from the rate: re-deriving a total
		// from a percentage is how a rounded line and a rounded total start to
		// differ by a centavo.
		TotalCents:          subtotal + fees,
		Currency:            currency,
		RefundPolicyVersion: draft.RefundPolicyVersion,
		Status:              StatusPendingPayment,
		HoldExpiresAt:       timestamp.Add(holdFor),
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
