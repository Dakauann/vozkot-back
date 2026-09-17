package pricing

import "testing"

func TestNewFeeRejectsImpossibleRates(t *testing.T) {
	for _, basisPoints := range []int{-1, MaxBasisPoints + 1, 10_000_0} {
		if _, err := NewFee(basisPoints); err == nil {
			t.Fatalf("NewFee(%d) was accepted; a rate outside 0-100%% is a typo, not a fee", basisPoints)
		}
	}
	fee, err := NewFee(PlatformBasisPoints)
	if err != nil {
		t.Fatalf("NewFee(%d): %v", PlatformBasisPoints, err)
	}
	if fee.BasisPoints != PlatformBasisPoints {
		t.Fatalf("basis points = %d, want %d", fee.BasisPoints, PlatformBasisPoints)
	}
}

func TestOnRoundsHalfUp(t *testing.T) {
	fee := Fee{BasisPoints: PlatformBasisPoints}
	cases := []struct {
		face int64
		want int64
	}{
		{0, 0},    // a free ticket stays free
		{100, 10}, // R$ 1,00 -> R$ 0,10
		{10_000, 1_000},
		{3_333, 333}, // 333.3 rounds down
		{3_335, 334}, // 333.5 rounds up, not to even
		{5, 1},       // half a centavo rounds up
		{4, 0},       // 0.4 rounds down
		{9_999, 1_000},
	}
	for _, testCase := range cases {
		if got := fee.On(testCase.face); got != testCase.want {
			t.Errorf("On(%d) = %d, want %d", testCase.face, got, testCase.want)
		}
	}
}

// The zero value must charge nothing. Every test, the load harness and any
// deployment that has not configured a fee relies on it.
func TestZeroFeeChargesNothing(t *testing.T) {
	var fee Fee
	if !fee.Free() {
		t.Fatal("the zero Fee reports itself as charging something")
	}
	if got := fee.On(50_000); got != 0 {
		t.Fatalf("On(50000) with no fee = %d, want 0", got)
	}
	quote := fee.Quote(50_000, 3)
	if quote.TotalCents != quote.FaceCents || quote.FeeCents != 0 {
		t.Fatalf("a zero fee changed the total: %+v", quote)
	}
}

// The per-unit fee times the quantity, never a fee on the line total. An event
// page that quotes one ticket has promised what three of them cost.
func TestQuoteMultipliesThePerUnitFee(t *testing.T) {
	fee := Fee{BasisPoints: PlatformBasisPoints}
	quote := fee.Quote(3_335, 3)

	if quote.UnitFeeCents != 334 {
		t.Fatalf("unit fee = %d, want 334", quote.UnitFeeCents)
	}
	if quote.FeeCents != 334*3 {
		t.Fatalf("line fee = %d, want %d; a fee taken on the line total would read %d",
			quote.FeeCents, 334*3, fee.On(3_335*3))
	}
	if quote.FaceCents != 3_335*3 {
		t.Fatalf("face = %d, want %d", quote.FaceCents, 3_335*3)
	}
	if quote.TotalCents != quote.FaceCents+quote.FeeCents {
		t.Fatalf("total %d is not face %d + fee %d", quote.TotalCents, quote.FaceCents, quote.FeeCents)
	}
}

// The organiser is owed the price they typed, whatever the fee is. This is the
// invariant the whole package exists for.
func TestFaceValueNeverMovesWithTheFee(t *testing.T) {
	const unit = 12_345
	const quantity = 7
	for _, basisPoints := range []int{0, 1, 500, PlatformBasisPoints, MaxBasisPoints} {
		fee := Fee{BasisPoints: basisPoints}
		quote := fee.Quote(unit, quantity)
		if quote.FaceCents != unit*quantity {
			t.Fatalf("at %d bp the organiser's share moved to %d, want %d",
				basisPoints, quote.FaceCents, unit*quantity)
		}
		if quote.TotalCents < quote.FaceCents {
			t.Fatalf("at %d bp the buyer pays %d, less than the face value %d",
				basisPoints, quote.TotalCents, quote.FaceCents)
		}
	}
}

func TestQuoteTreatsNegativeQuantityAsNone(t *testing.T) {
	fee := Fee{BasisPoints: PlatformBasisPoints}
	quote := fee.Quote(10_000, -3)
	if quote.TotalCents != 0 || quote.FaceCents != 0 || quote.FeeCents != 0 {
		t.Fatalf("a negative quantity produced money: %+v", quote)
	}
}
