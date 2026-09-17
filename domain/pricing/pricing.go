// Package pricing is what a ticket costs the buyer versus what it pays the
// organiser.
//
// Those are two different numbers and the whole package exists to keep them
// apart. The organiser prices a tier at R$ 100; the box office adds its
// commission ON TOP, so the buyer pays R$ 110 and the organiser is still owed
// exactly the R$ 100 they asked for. The alternative — deducting the fee from
// the face value — is the same arithmetic and a different promise, and it is
// the one that makes an organiser's payout disagree with the price they typed.
//
// Nothing here knows about orders, tiers or providers. It is the arithmetic
// alone, so the checkout that charges it, the event page that quotes it and the
// report that reconciles it cannot each round it their own way.
package pricing

import "errors"

// BasisPointsPerUnit is 100%. Fees are held in basis points rather than as a
// percentage because a percent is a float the moment anyone divides by it, and
// a float that reaches money is a centavo that goes missing at scale.
const BasisPointsPerUnit = 10_000

// PlatformBasisPoints is the commission this box office charges: ten per cent,
// written here in code and nowhere else.
//
// In code rather than in configuration, on purpose. A rate that arrives from
// the environment has two failure modes that a constant does not: a deployment
// that forgets the variable silently charges nothing and quietly eats the
// commission on every sale until somebody reconciles a payout, and a
// deployment that fat-fingers it overcharges real buyers. Neither is
// recoverable after the fact, because the money has already moved. A constant
// makes the rate a property of the build: it ships with the code that was
// reviewed, and the only way to change it is to change this line.
//
// It is still run through NewFee at startup rather than trusted, so the bound
// below is enforced against this value too and an edit here that is out of
// range stops the process instead of reaching a buyer.
const PlatformBasisPoints = 1_000

// MaxBasisPoints bounds what an operator can configure.
//
// A fee above the ticket itself is not a fee, it is a typo — almost always an
// extra zero — and the cost of accepting one is every buyer being charged
// double before anybody notices. A ceiling turns that into a refusal to start.
const MaxBasisPoints = BasisPointsPerUnit

var ErrInvalidBasisPoints = errors.New("service fee must be between 0 and 100 per cent")

// Fee is the commission the box office adds to a face value.
//
// The zero value charges nothing, which is what every test, the load harness
// and any deployment that has not configured a fee gets. That matters: a fee
// that defaults to something is a fee that appears on a charge nobody meant to
// add one to.
type Fee struct {
	BasisPoints int
}

// NewFee validates an operator-supplied rate.
func NewFee(basisPoints int) (Fee, error) {
	if basisPoints < 0 || basisPoints > MaxBasisPoints {
		return Fee{}, ErrInvalidBasisPoints
	}
	return Fee{BasisPoints: basisPoints}, nil
}

// Free reports whether this fee adds nothing.
func (f Fee) Free() bool { return f.BasisPoints <= 0 }

// PlatformFee is the commission this deployment charges.
//
// The one constructor anything outside this package should use. Config, the
// container, the event page and the checkout all read the fee from here, so
// there is exactly one answer to "what do we charge" and no call site can hold
// a different one.
func PlatformFee() (Fee, error) { return NewFee(PlatformBasisPoints) }

// On is the fee owed on ONE ticket at faceCents.
//
// Per unit, and then multiplied — never taken on the line total. The two differ
// by up to a centavo per ticket, and only the per-unit number is the one a
// buyer can check: an event page that says "R$ 100,00 + R$ 10,00 de taxa" has
// promised that three of them cost R$ 330,00, and a line-total rounding that
// answered R$ 329,99 would have made the page a lie for the sake of a centavo.
//
// Rounding is half-up, which is what every Brazilian price list does and what
// the buyer will compute by hand if they check.
//
// A free ticket is free. Ten per cent of nothing is nothing, so the guard below
// is arithmetic rather than policy — but it is worth saying out loud, because a
// free-admission event that started charging a service fee would be the single
// most visible way this package could be wrong.
func (f Fee) On(faceCents int64) int64 {
	if f.Free() || faceCents <= 0 {
		return 0
	}
	// +half the divisor before the integer division is round-half-up, done in
	// integers so no float ever touches a price.
	return (faceCents*int64(f.BasisPoints) + BasisPointsPerUnit/2) / BasisPointsPerUnit
}

// Breakdown is one priced line: what the organiser earns, what the box office
// takes, and what the buyer is charged.
//
// All three are carried rather than derived at each call site, because the
// receipt, the charge, the payout report and the refund all need the same
// split and each deriving it again is how two of them start to disagree.
type Breakdown struct {
	// FaceCents is the organiser's share: the price they set, times quantity.
	FaceCents int64
	// FeeCents is the box office's commission.
	FeeCents int64
	// TotalCents is what the buyer pays. Always FaceCents + FeeCents.
	TotalCents int64
	// UnitFaceCents and UnitFeeCents are the per-ticket halves, kept so a page
	// can quote one ticket without dividing a total back down.
	UnitFaceCents int64
	UnitFeeCents  int64
}

// Quote prices `quantity` tickets at `unitFaceCents`.
func (f Fee) Quote(unitFaceCents int64, quantity int) Breakdown {
	if quantity < 0 {
		quantity = 0
	}
	unitFee := f.On(unitFaceCents)
	face := unitFaceCents * int64(quantity)
	fee := unitFee * int64(quantity)
	return Breakdown{
		FaceCents:     face,
		FeeCents:      fee,
		TotalCents:    face + fee,
		UnitFaceCents: unitFaceCents,
		UnitFeeCents:  unitFee,
	}
}
