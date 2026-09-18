package ledger

import (
	"errors"
	"time"
)

// Terms are the payout rules applied to one organiser at the moment a sale
// accrues, and then frozen onto the entries it produced.
//
// Frozen for the same reason the refund policy and the unit price are: an
// organiser promoted to better terms on Tuesday must not have their Monday
// sales silently re-dated, and one demoted after a chargeback must not have
// money they were already promised pulled back into a reserve.
type Terms struct {
	// ReserveBasisPoints is the slice of each sale withheld past settlement.
	// Zero is a trusted organiser and writes no reserve entry at all.
	ReserveBasisPoints int
	// SettlementBusinessDays is how long after the show the money is due.
	SettlementBusinessDays int
	// ReserveHold is how much longer the withheld slice waits, measured from
	// the settlement date.
	ReserveHold time.Duration
}

// The three tiers the plan proposes, named so a risk decision is a tier change
// rather than four numbers typed at a call site.
//
// The reserve exists for exactly one reason: Pix's Mecanismo Especial de
// Devolução lets a payer claim fraud for up to 80 days AFTER THE TRANSFER, not
// after the show. Pix is the only instrument this box office issues, so the
// tail risk on a paid order runs past the event by as much as the buyer bought
// early, which is why StandardTerms holds its slice for 30 days past
// settlement and NewTerms, with no history to judge, holds more for longer.
var (
	NewTerms      = Terms{ReserveBasisPoints: 2_000, SettlementBusinessDays: 3, ReserveHold: 45 * 24 * time.Hour}
	StandardTerms = Terms{ReserveBasisPoints: 1_000, SettlementBusinessDays: 3, ReserveHold: 30 * 24 * time.Hour}
	TrustedTerms  = Terms{ReserveBasisPoints: 0, SettlementBusinessDays: 1, ReserveHold: 0}
)

// BasisPointsPerUnit is 100%, in the same integer units domain/pricing uses so
// the two cannot disagree about what a percentage is.
const BasisPointsPerUnit = 10_000

var ErrInvalidTerms = errors.New("ledger: reserve must be between 0 and 100 per cent and settlement at least one day")

func (t Terms) Validate() error {
	if t.ReserveBasisPoints < 0 || t.ReserveBasisPoints > BasisPointsPerUnit {
		return ErrInvalidTerms
	}
	if t.SettlementBusinessDays < 1 {
		return ErrInvalidTerms
	}
	return nil
}

// reserveOn is the withheld slice of one amount, rounded DOWN.
//
// Down rather than half-up, which is the opposite of the service fee's
// rounding and deliberately so: the fee rounds towards the buyer being quoted a
// number they can check, and this rounds towards the organiser keeping the odd
// centavo rather than the platform withholding it. Neither is arbitrary and
// neither should be copied from the other.
func (t Terms) reserveOn(amountCents int64) int64 {
	if t.ReserveBasisPoints <= 0 || amountCents <= 0 {
		return 0
	}
	return amountCents * int64(t.ReserveBasisPoints) / BasisPointsPerUnit
}

// Sale is one paid order, reduced to what the ledger needs.
//
// A struct rather than domain/order so this package depends on nothing but the
// standard library, and so the caller has to state the organiser explicitly:
// an order knows its event, and the event knows its owner, and quietly walking
// that chain inside here would hide a join from the place that has to do it in
// one query.
type Sale struct {
	OrderID string
	EventID string
	// OrganiserID is the event's owner: whoever the money is owed to.
	OrganiserID string
	// FaceCents is the organiser's share of the order, the sum of the lines'
	// face values, which is order.SubtotalCents. NOT what the buyer paid: the
	// service fee on top was never the organiser's and does not belong here.
	FaceCents int64
	// EventEndsAt anchors the settlement date. Nobody is paid for a show that
	// has not happened, because until the doors open the whole gross is a
	// potential refund.
	EventEndsAt time.Time
}

var (
	ErrNoSaleOrder   = errors.New("ledger: an accrual must name its order")
	ErrNoSaleEvent   = errors.New("ledger: an accrual must name its event and organiser")
	ErrNoSaleEnd     = errors.New("ledger: an accrual needs the event's end, which anchors settlement")
	ErrSaleNotWorth  = errors.New("ledger: an order worth nothing accrues nothing")
	ErrRefundTooMuch = errors.New("ledger: a reversal cannot exceed what the order accrued")
)

// Accrue turns one paid order into the entries it owes the organiser.
//
// Pure, and the only place the split between due-now and held-back is decided.
// The settle path calls it inside the transaction that marks the order paid,
// because an accrual that can exist without its payment, or be lost for one
// that committed, is a ledger that has to be reconciled by hand forever.
//
// `now` is the accrual instant and `calendar` decides which days the banks are
// open AND whose days those are; both are passed rather than read so the same
// call in a test on a Tuesday and in production on a Saturday answer the same
// way.
func Accrue(sale Sale, terms Terms, now time.Time, calendar Calendar, id func() string) ([]Entry, error) {
	if sale.OrderID == "" {
		return nil, ErrNoSaleOrder
	}
	if sale.EventID == "" || sale.OrganiserID == "" {
		return nil, ErrNoSaleEvent
	}
	if sale.EventEndsAt.IsZero() {
		return nil, ErrNoSaleEnd
	}
	if sale.FaceCents <= 0 {
		return nil, ErrSaleNotWorth
	}
	if err := terms.Validate(); err != nil {
		return nil, err
	}

	settlesAt := calendar.AddBusinessDays(sale.EventEndsAt, terms.SettlementBusinessDays)
	withheld := terms.reserveOn(sale.FaceCents)
	due := sale.FaceCents - withheld

	entries := make([]Entry, 0, 2)
	if due > 0 {
		entries = append(entries, Entry{
			ID: id(), OrganiserID: sale.OrganiserID, EventID: sale.EventID, OrderID: sale.OrderID,
			Kind: KindSale, AmountCents: due, AvailableAt: settlesAt, CreatedAt: now,
		})
	}
	if withheld > 0 {
		entries = append(entries, Entry{
			ID: id(), OrganiserID: sale.OrganiserID, EventID: sale.EventID, OrderID: sale.OrderID,
			Kind: KindReserve, AmountCents: withheld,
			AvailableAt: settlesAt.Add(terms.ReserveHold), CreatedAt: now,
		})
	}
	for _, entry := range entries {
		if err := entry.Validate(); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// Reverse is the negative pair of an accrual: a refund, or a chargeback.
//
// Available IMMEDIATELY, whatever the sale's dates were. That asymmetry is the
// safety property of this package: see the package comment. It is why
// this takes `now` as the availability rather than deriving one.
//
// `accruedCents` is what the order actually put on the ledger, so a reversal
// can be checked against it rather than against what the order says it was
// worth. The two differ the moment anything has already been reversed, and
// reversing more than was ever accrued would credit the platform's own money
// against the organiser.
func Reverse(sale Sale, kind Kind, amountCents, accruedCents int64, now time.Time, id func() string) (Entry, error) {
	if kind != KindRefund && kind != KindChargeback {
		return Entry{}, ErrInvalidKind
	}
	if sale.OrderID == "" {
		return Entry{}, ErrNoSaleOrder
	}
	if sale.EventID == "" || sale.OrganiserID == "" {
		return Entry{}, ErrNoSaleEvent
	}
	if amountCents <= 0 {
		return Entry{}, ErrSaleNotWorth
	}
	if amountCents > accruedCents {
		return Entry{}, ErrRefundTooMuch
	}
	entry := Entry{
		ID: id(), OrganiserID: sale.OrganiserID, EventID: sale.EventID, OrderID: sale.OrderID,
		Kind: kind, AmountCents: -amountCents, AvailableAt: now, CreatedAt: now,
	}
	return entry, entry.Validate()
}
