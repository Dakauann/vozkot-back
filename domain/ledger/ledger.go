// Package ledger is what each organiser has earned and when it becomes theirs
// to take.
//
// An append-only list of signed amounts, and that shape is the whole design. A
// balance column would be one number that every concurrent write races for and
// that nobody can explain a month later; a sum over rows is arithmetic anybody
// can re-run, and a correction is a new row rather than an edit to a number
// that has already been reported.
//
// ONE INVARIANT HOLDS THE REST UP: an entry's AvailableAt says when its money
// may leave, and money can always leave the balance FASTER than it can enter
// it. A sale becomes available days after the show; a refund is available the
// instant it is written. So a payout run can never pay out money that has
// already gone back to a buyer: the refund is already counted against the
// balance while the sale it reverses is not yet in it.
//
// WHAT THIS LEDGER IS NOT: it is not the platform's books. It records what the
// ORGANISER is owed, and the service fee was never theirs: pricing adds it on
// top of their price rather than taking it out, so a `platform_fee` row here
// would be subtracting money that was never added. Platform revenue is a
// different question, already answered by the sales report. Keeping this ledger
// to one subject is what lets SUM(amount_cents) be the balance with no filter,
// no sign convention to remember, and no way to read it wrong.
package ledger

import (
	"errors"
	"time"
)

// Kind is what an entry records. Stored, never inferred from the sign.
type Kind string

const (
	// KindSale is the organiser's share of one paid order, less whatever was
	// withheld as reserve. Positive, available on the settlement date.
	KindSale Kind = "sale"
	// KindReserve is the withheld slice of the same order, held against the
	// long tail of Pix reversals. Positive, available later than the sale.
	KindReserve Kind = "reserve"
	// KindRefund reverses an order's claim. Negative, available immediately.
	KindRefund Kind = "refund"
	// KindChargeback is a Pix MED reversal. Negative, available immediately.
	KindChargeback Kind = "chargeback"
	// KindGatewayFee is the acquirer's cost on a movement, billed to whoever
	// bears it. Negative when it is the organiser's to carry.
	KindGatewayFee Kind = "gateway_fee"
	// KindPayout is money that has left. Negative, written when a payout is
	// created so the same balance cannot be paid out twice.
	KindPayout Kind = "payout"
	// KindAdjustment is a person, with a note and an author.
	KindAdjustment Kind = "adjustment"
)

func (k Kind) Valid() bool {
	switch k {
	case KindSale, KindReserve, KindRefund, KindChargeback,
		KindGatewayFee, KindPayout, KindAdjustment:
		return true
	}
	return false
}

// Withheld reports whether this kind is money held back rather than merely not
// yet due. The two read differently to an organiser looking at their balance:
// "arrives on the 3rd" and "held until the 2nd of next month" are different
// sentences and hiding the difference is how a reserve becomes a support ticket.
func (k Kind) Withheld() bool { return k == KindReserve }

var (
	ErrInvalidKind    = errors.New("ledger: entry kind is not one of the supported kinds")
	ErrNoOrganiser    = errors.New("ledger: an entry must name the organiser it belongs to")
	ErrZeroAmount     = errors.New("ledger: an entry of zero moves nothing and must not be written")
	ErrNoAvailability = errors.New("ledger: an entry must say when its money becomes available")
)

// Entry is one movement on one organiser's balance. Append-only: never updated,
// never deleted.
type Entry struct {
	ID          string
	OrganiserID string
	// EventID is carried so an organiser can read the ledger of one show
	// without joining through orders, which is the query their financial page
	// runs on every load.
	EventID string
	// OrderID is empty on an adjustment or a payout, which belong to the
	// organiser rather than to any one sale.
	OrderID string
	Kind    Kind
	// AmountCents is SIGNED and is the only place the direction of the money
	// lives. A caller that writes a positive refund has written a credit, and
	// no amount of naming would have caught it, so Validate refuses the pairs
	// that cannot be right.
	AmountCents int64
	// AvailableAt is when this money may be paid out. See the package comment:
	// it is the field the whole design rests on.
	AvailableAt time.Time
	CreatedAt   time.Time
	// Note carries the human reason on an adjustment. Empty elsewhere.
	Note string
}

// Validate refuses an entry that could not be right.
//
// The sign checks are the ones worth having: every mistake this package could
// make that a test would not obviously catch is a credit written where a debit
// belonged, and a positive refund would silently pay an organiser for a ticket
// the buyer got their money back for.
func (e Entry) Validate() error {
	if !e.Kind.Valid() {
		return ErrInvalidKind
	}
	if e.OrganiserID == "" {
		return ErrNoOrganiser
	}
	if e.AmountCents == 0 {
		return ErrZeroAmount
	}
	if e.AvailableAt.IsZero() {
		return ErrNoAvailability
	}
	switch e.Kind {
	case KindSale, KindReserve:
		if e.AmountCents < 0 {
			return errors.New("ledger: a sale or reserve credits the organiser and cannot be negative")
		}
	case KindRefund, KindChargeback, KindGatewayFee, KindPayout:
		if e.AmountCents > 0 {
			return errors.New("ledger: a refund, chargeback, fee or payout takes money and cannot be positive")
		}
	}
	return nil
}

// Balance is one organiser's position, split the way they need to read it.
//
// Every field is derived from the same rows by the same sum; they differ only
// in which rows they include. Nothing here is stored.
type Balance struct {
	// AvailableCents is what could be paid out right now: every entry whose
	// AvailableAt has passed. It CAN be negative, and that is not a bug: a
	// refund that outran its sale is exactly the case this ledger exists to
	// make visible rather than to pay out.
	AvailableCents int64
	// PendingCents is sales not yet due.
	PendingCents int64
	// ReservedCents is the withheld slice, not yet due.
	ReservedCents int64
	// TotalCents is everything, due or not: the organiser's whole claim.
	TotalCents int64
}

// BalanceOf sums entries into a position as of `now`.
//
// A pure function over rows so the same arithmetic serves the API, a report and
// a test, and so the database's aggregate can be checked against it. The
// repository runs this as SQL for the real thing; this is the definition that
// SQL has to agree with, and the test that proves they do is worth more than
// either.
func BalanceOf(entries []Entry, now time.Time) Balance {
	var balance Balance
	for _, entry := range entries {
		balance.TotalCents += entry.AmountCents
		switch {
		case !entry.AvailableAt.After(now):
			balance.AvailableCents += entry.AmountCents
		case entry.Kind.Withheld():
			balance.ReservedCents += entry.AmountCents
		default:
			balance.PendingCents += entry.AmountCents
		}
	}
	return balance
}
