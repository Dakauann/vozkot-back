package seating

import "testing"

// The suggestion exists because the studio told organisers to "mark the seats"
// and gave them nothing to mark them with. These protect the rule it uses.

func room(t *testing.T, rows, perRow int) []Seat {
	t.Helper()
	seats, err := RowSpec{Rows: rows, SeatsPerRow: perRow}.Generate("sec_a")
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	return seats
}

func TestSuggestionSatisfiesEveryShortfall(t *testing.T) {
	seats := room(t, 10, 20) // 200 places: 4 of each group, 2 obese.
	report := CheckCompliance(seats, 0)
	if report.Compliant() {
		t.Fatal("a plain room should not already be compliant")
	}

	suggestion := SuggestAccessibleSeats(seats, report)
	if len(suggestion) == 0 {
		t.Fatal("nothing was suggested for a room short of everything")
	}

	// Apply it and the room must pass.
	applied := make([]Seat, len(seats))
	copy(applied, seats)
	for index := range applied {
		key := SeatKindKey(applied[index].RowLabel, applied[index].SeatLabel)
		if kind, ok := suggestion[key]; ok {
			applied[index].Kind = kind
		}
	}
	after := CheckCompliance(applied, 0)
	if !after.Compliant() {
		t.Errorf("after applying the suggestion the room is still short: %+v", after)
	}
}

// The back row, because that is where a wheelchair reaches without crossing
// anybody, and because the front rows are the ones an organiser sells dearest.
func TestSuggestionTakesTheBackRow(t *testing.T) {
	seats := room(t, 6, 12)
	report := CheckCompliance(seats, 0)
	suggestion := SuggestAccessibleSeats(seats, report)

	// The row label of the deepest row, whatever the lettering produced.
	last := ""
	for _, seat := range seats {
		if seat.RowOrder == 5 {
			last = seat.RowLabel
			break
		}
	}
	if last == "" {
		t.Fatal("the room has no sixth row")
	}
	for key := range suggestion {
		if key[:len(last)+1] != last+"/" {
			t.Errorf("suggested %q, which is not in the back row %q", key, last)
		}
	}
}

// A wheelchair space with nobody beside it is not usable, and the decree
// requires the pair.
func TestSuggestionPairsEachSpaceWithACompanion(t *testing.T) {
	seats := room(t, 8, 16)
	report := CheckCompliance(seats, 0)
	suggestion := SuggestAccessibleSeats(seats, report)

	spaces, companions := 0, 0
	for _, kind := range suggestion {
		switch kind {
		case SeatWheelchair:
			spaces++
		case SeatCompanion:
			companions++
		}
	}
	if spaces == 0 {
		t.Fatal("no wheelchair spaces were suggested")
	}
	if companions < spaces {
		t.Errorf("%d wheelchair spaces got %d companion seats", spaces, companions)
	}
}

// Only the shortfall. A room already carrying half its spaces is not asked to
// double them.
func TestSuggestionOnlyCoversWhatIsMissing(t *testing.T) {
	seats := room(t, 10, 20)
	report := CheckCompliance(seats, 0)
	required := report.RequiredWheelchair

	// Mark one wheelchair space by hand.
	seats[0].Kind = SeatWheelchair
	partial := CheckCompliance(seats, 0)
	suggestion := SuggestAccessibleSeats(seats, partial)

	spaces := 0
	for _, kind := range suggestion {
		if kind == SeatWheelchair {
			spaces++
		}
	}
	if spaces != required-1 {
		t.Errorf("suggested %d spaces on top of 1 already marked, want %d", spaces, required-1)
	}
}

// A chair the organiser already marked is never overwritten: their knowledge of
// the room beats this function's rule.
func TestSuggestionNeverOverwritesAMarkedSeat(t *testing.T) {
	seats := room(t, 4, 10)
	// Mark the whole back row as something deliberate.
	for index := range seats {
		if seats[index].RowOrder == 3 {
			seats[index].Kind = SeatRestrictedView
		}
	}
	report := CheckCompliance(seats, 0)
	suggestion := SuggestAccessibleSeats(seats, report)

	for _, seat := range seats {
		if seat.Kind != SeatRestrictedView {
			continue
		}
		if _, ok := suggestion[SeatKindKey(seat.RowLabel, seat.SeatLabel)]; ok {
			t.Errorf("suggestion overwrote seat %s%s, which was already marked",
				seat.RowLabel, seat.SeatLabel)
		}
	}
}

func TestSuggestionOnACompliantRoomIsEmpty(t *testing.T) {
	seats := room(t, 4, 10)
	report := CheckCompliance(seats, 0)
	suggestion := SuggestAccessibleSeats(seats, report)

	applied := make([]Seat, len(seats))
	copy(applied, seats)
	for index := range applied {
		if kind, ok := suggestion[SeatKindKey(applied[index].RowLabel, applied[index].SeatLabel)]; ok {
			applied[index].Kind = kind
		}
	}
	// Asking again once it passes suggests nothing.
	again := SuggestAccessibleSeats(applied, CheckCompliance(applied, 0))
	if len(again) != 0 {
		t.Errorf("a compliant room was still offered %d changes", len(again))
	}
}

func TestSuggestionOnAnEmptyRoomIsEmpty(t *testing.T) {
	if got := SuggestAccessibleSeats(nil, Compliance{}); len(got) != 0 {
		t.Errorf("an empty room was offered %d changes", len(got))
	}
}
