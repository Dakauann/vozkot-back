package seating

import "testing"

// The generator is a pure function, so these are the only tests in this feature
// that need no database. That is the point of putting it in the domain: the
// labels and the adjacency a whole house depends on can be asserted directly.

func TestGenerateLettersRowsSkippingI(t *testing.T) {
	// Ten rows must reach K, not J, because I is skipped. A house whose
	// generated map said I would not match the letters painted on its floor.
	seats, err := RowSpec{Rows: 10, SeatsPerRow: 2}.Generate("sec_1")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	rows := map[string]bool{}
	for _, seat := range seats {
		rows[seat.RowLabel] = true
	}
	if rows["I"] {
		t.Error("the generator produced a row I")
	}
	for _, want := range []string{"A", "H", "J", "K"} {
		if !rows[want] {
			t.Errorf("row %s is missing from a ten-row block", want)
		}
	}
	if len(rows) != 10 {
		t.Errorf("a ten-row block has %d distinct rows", len(rows))
	}
}

func TestGenerateSkipsAislePositions(t *testing.T) {
	seats, err := RowSpec{Rows: 1, SeatsPerRow: 10, Skips: []int{3, 8}}.Generate("sec_1")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if len(seats) != 8 {
		t.Fatalf("ten seats with two aisles produced %d", len(seats))
	}
	for _, seat := range seats {
		if seat.SeatLabel == "3" || seat.SeatLabel == "8" {
			t.Errorf("seat %s was supposed to be an aisle", seat.SeatLabel)
		}
	}
}

// Odd/even numbering, and the reason adjacency is never read off a label.
func TestOddEvenNumberingKeepsNeighboursAdjacent(t *testing.T) {
	seats, err := RowSpec{Rows: 1, SeatsPerRow: 6, Numbering: NumberingOddEven}.Generate("sec_1")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	labels := make([]string, 0, len(seats))
	for _, seat := range seats {
		labels = append(labels, seat.SeatLabel)
	}
	// Outward from the centre: odds to the left, evens to the right.
	want := []string{"5", "3", "1", "2", "4", "6"}
	for index := range want {
		if labels[index] != want[index] {
			t.Fatalf("labels = %v, want %v", labels, want)
		}
	}
	// Seats 1 and 2 are next to each other across the centre; 5 and 3 are next
	// to each other on the left. Neither fact is derivable from the numbers,
	// which is why SeatOrder exists.
	for index := 1; index < len(seats); index++ {
		if seats[index].SeatOrder != seats[index-1].SeatOrder+1 {
			t.Errorf("seat order broke between %s and %s",
				seats[index-1].SeatLabel, seats[index].SeatLabel)
		}
	}
}

func TestGenerateMarksAccessibleSeats(t *testing.T) {
	seats, err := RowSpec{
		Rows:        2,
		SeatsPerRow: 4,
		KindByLabel: map[string]SeatKind{
			SeatKindKey("A", "1"): SeatWheelchair,
			SeatKindKey("A", "2"): SeatCompanion,
			SeatKindKey("B", "4"): SeatObese,
		},
	}.Generate("sec_1")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	found := map[string]SeatKind{}
	for _, seat := range seats {
		found[SeatKindKey(seat.RowLabel, seat.SeatLabel)] = seat.Kind
	}
	if found["A/1"] != SeatWheelchair {
		t.Errorf("A/1 is %q, want wheelchair", found["A/1"])
	}
	if found["A/2"] != SeatCompanion {
		t.Errorf("A/2 is %q, want companion", found["A/2"])
	}
	if found["B/4"] != SeatObese {
		t.Errorf("B/4 is %q, want obese", found["B/4"])
	}
	if found["B/1"] != SeatStandard {
		t.Errorf("an unmarked seat is %q, want standard", found["B/1"])
	}
}

func TestGenerateRefusesMoreRowsThanLetters(t *testing.T) {
	if _, err := (RowSpec{Rows: 40, SeatsPerRow: 2}).Generate("sec_1"); err == nil {
		t.Fatal("forty rows were generated from twenty-four letters")
	}
}

// The accessibility quotas, which are the law and not a preference.
//
// Decreto 5.296/2004 art. 23, as amended by Decreto 9.404/2018.
func TestComplianceQuotasFollowTheDecree(t *testing.T) {
	cases := []struct {
		name                string
		capacity            int
		wantWheelchair      int
		wantReducedMobility int
		wantObese           int
	}{
		// 2% of capacity, rounded up, for a room up to a thousand places.
		{"a 500-seat theatre", 500, 10, 10, 5},
		{"a 1000-seat house", 1000, 20, 20, 10},
		// A small room still owes at least one of each, and at least one of
		// those must be built for an obese person.
		{"a 30-seat studio", 30, 1, 1, 1},
		// Above a thousand the scale changes to 20 + 1% of the excess, which is
		// what stops a stadium owing 1,200 wheelchair spaces.
		{"a 5000-seat arena", 5000, 60, 60, 30},
		{"a 60000-seat stadium", 60000, 610, 610, 305},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			seats := make([]Seat, testCase.capacity)
			report := CheckCompliance(seats, 0)
			if report.RequiredWheelchair != testCase.wantWheelchair {
				t.Errorf("wheelchair spaces required = %d, want %d",
					report.RequiredWheelchair, testCase.wantWheelchair)
			}
			if report.RequiredReducedMobility != testCase.wantReducedMobility {
				t.Errorf("reduced-mobility seats required = %d, want %d",
					report.RequiredReducedMobility, testCase.wantReducedMobility)
			}
			if report.RequiredObese != testCase.wantObese {
				t.Errorf("obese seats required = %d, want %d",
					report.RequiredObese, testCase.wantObese)
			}
		})
	}
}

// A room that meets every quota reports compliant; one short of a companion
// seat does not, because a wheelchair space without one is not usable.
func TestComplianceNeedsACompanionPerWheelchairSpace(t *testing.T) {
	build := func(companions int) []Seat {
		seats := make([]Seat, 0, 60)
		for index := 0; index < 55; index++ {
			seats = append(seats, Seat{Kind: SeatStandard})
		}
		// A 60-place room owes 2 of each; obese is half of that, minimum 1.
		seats = append(seats, Seat{Kind: SeatWheelchair}, Seat{Kind: SeatWheelchair})
		seats = append(seats, Seat{Kind: SeatObese}, Seat{Kind: SeatReducedMobility})
		for index := 0; index < companions; index++ {
			seats = append(seats, Seat{Kind: SeatCompanion})
		}
		return seats
	}

	if report := CheckCompliance(build(2), 0); !report.Compliant() {
		t.Errorf("a compliant room reported non-compliant: %+v", report)
	}
	if report := CheckCompliance(build(1), 0); report.Compliant() {
		t.Error("a room with two wheelchair spaces and one companion seat reported compliant")
	}
}

// An obese seat counts towards the reduced-mobility quota, because the decree
// makes it a subset of that group rather than a separate requirement.
func TestObeseSeatsCountTowardsReducedMobility(t *testing.T) {
	seats := make([]Seat, 0, 100)
	for index := 0; index < 96; index++ {
		seats = append(seats, Seat{Kind: SeatStandard})
	}
	seats = append(seats,
		Seat{Kind: SeatWheelchair}, Seat{Kind: SeatWheelchair},
		Seat{Kind: SeatObese}, Seat{Kind: SeatObese})

	report := CheckCompliance(seats, 0)
	if report.HaveReducedMobility != 2 {
		t.Errorf("two obese seats counted as %d reduced-mobility seats, want 2",
			report.HaveReducedMobility)
	}
	if report.HaveObese != 2 {
		t.Errorf("obese seats = %d, want 2", report.HaveObese)
	}
}

// Standing room counts towards capacity, so a Pista raises what the seated part
// of a mixed house owes.
func TestStandingCapacityCountsTowardsTheQuota(t *testing.T) {
	seats := make([]Seat, 100)
	withoutPista := CheckCompliance(seats, 0)
	withPista := CheckCompliance(seats, 900)

	if withoutPista.RequiredWheelchair != 2 {
		t.Errorf("a 100-place room requires %d wheelchair spaces, want 2",
			withoutPista.RequiredWheelchair)
	}
	if withPista.RequiredWheelchair != 20 {
		t.Errorf("a 100-seat room with 900 standing requires %d, want 20",
			withPista.RequiredWheelchair)
	}
}
