package seating

import (
	"math"
	"testing"
)

// The round venues: a rodeo arena, and a floor of tables.
//
// Both are pure geometry, so these tests assert on coordinates directly. What
// they are really protecting is that the LABELS, the ordering and the adjacency
// come out identical to a theatre's, because everything downstream of the
// generator (the claim, "best available", the door) reads those and must not
// have to know what shape the room is.

// distance from the arena's centre, which is always the origin.
func radiusOf(seat Seat) float64 { return math.Hypot(seat.X, seat.Y) }

func TestArcRowsSitOnConcentricCircles(t *testing.T) {
	seats, err := RowSpec{
		Shape:       ShapeArc,
		Rows:        3,
		SeatsPerRow: 8,
		Radius:      100,
		RowGap:      20,
		SweepAngle:  90,
		RowLabels:   RowsNumbered,
	}.Generate("sec_arena")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if len(seats) != 24 {
		t.Fatalf("a 3x8 arc produced %d seats", len(seats))
	}

	// Every seat of a row is the same distance from the middle. That is what
	// makes it a stand rather than a diagonal.
	want := map[int]float64{0: 100, 1: 120, 2: 140}
	for _, seat := range seats {
		if got := radiusOf(seat); math.Abs(got-want[seat.RowOrder]) > 0.001 {
			t.Errorf("row %d seat %s sits at radius %.3f, want %.0f",
				seat.RowOrder, seat.SeatLabel, got, want[seat.RowOrder])
		}
	}
}

// Zero degrees is straight up and ninety is to the right, in screen
// coordinates where y grows downward. A venue describes its own stands by the
// clock, and getting the sign wrong mirrors the whole room.
func TestArcBearingsRunClockwiseFromTheTop(t *testing.T) {
	seats, err := RowSpec{
		Shape:       ShapeArc,
		Rows:        1,
		SeatsPerRow: 5,
		Radius:      100,
		StartAngle:  0,
		SweepAngle:  360,
	}.Generate("sec_ring")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Seat 1 at the top: x ~ 0, y ~ -100 (up the screen).
	if math.Abs(seats[0].X) > 0.001 || math.Abs(seats[0].Y+100) > 0.001 {
		t.Errorf("the first seat of a ring is at (%.3f, %.3f), want (0, -100)",
			seats[0].X, seats[0].Y)
	}
	// A quarter of the way round a 360 sweep of five seats is 72 degrees, so
	// the second seat is up and to the RIGHT: positive x, still negative y.
	if seats[1].X <= 0 || seats[1].Y >= 0 {
		t.Errorf("the second seat is at (%.3f, %.3f); a clockwise sweep puts it up and right",
			seats[1].X, seats[1].Y)
	}
}

// A full ring must not stack its last seat on its first.
//
// The off-by-one that does it is dividing the sweep by n-1 instead of n, which
// is correct for a partial sector and wrong for a closed circle.
func TestAFullRingDoesNotDoubleUpItsSeam(t *testing.T) {
	seats, err := RowSpec{
		Shape: ShapeArc, Rows: 1, SeatsPerRow: 12, Radius: 80, SweepAngle: 360,
	}.Generate("sec_ring")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	first, last := seats[0], seats[len(seats)-1]
	if math.Hypot(first.X-last.X, first.Y-last.Y) < 1 {
		t.Errorf("the first and last seat of a ring are both at (%.2f, %.2f)", first.X, first.Y)
	}
}

// A partial sector reaches both of its edges, which is the opposite rule.
func TestAPartialSectorSpansItsWholeSweep(t *testing.T) {
	seats, err := RowSpec{
		Shape: ShapeArc, Rows: 1, SeatsPerRow: 3, Radius: 100,
		StartAngle: 0, SweepAngle: 180,
	}.Generate("sec_half")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	// First at the top, last at the bottom, middle to the right.
	if math.Abs(seats[0].Y+100) > 0.001 {
		t.Errorf("the first seat is at y=%.3f, want -100 (the top)", seats[0].Y)
	}
	if math.Abs(seats[2].Y-100) > 0.001 {
		t.Errorf("the last seat is at y=%.3f, want +100 (the bottom)", seats[2].Y)
	}
	if seats[1].X <= 0 {
		t.Errorf("the middle seat is at x=%.3f, want positive (the right)", seats[1].X)
	}
}

// The thing that matters most: an arc is ordinary downstream.
//
// Adjacency, ordering and labels are what the claim, "best available" and the
// door read, and none of them may need to know the room is round.
func TestAnArcIsIndistinguishableDownstream(t *testing.T) {
	seats, err := RowSpec{
		Shape: ShapeArc, Rows: 2, SeatsPerRow: 6, Radius: 90,
		SweepAngle: 120, RowLabels: RowsNumbered,
	}.Generate("sec_arena")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	for index, seat := range seats {
		if seat.RowLabel == "" || seat.SeatLabel == "" {
			t.Fatalf("seat %d has no label: %#v", index, seat)
		}
	}
	// Rows numbered from one, not lettered, which is what an arena does.
	if seats[0].RowLabel != "1" {
		t.Errorf("the first row of a numbered arc is %q, want \"1\"", seats[0].RowLabel)
	}
	// Seat orders run 1..6 within a row, so a run of adjacent seats is a walk.
	for index := 1; index < 6; index++ {
		if seats[index].SeatOrder != seats[index-1].SeatOrder+1 {
			t.Errorf("seat order broke between %s and %s",
				seats[index-1].SeatLabel, seats[index].SeatLabel)
		}
		if seats[index].RowOrder != seats[0].RowOrder {
			t.Errorf("seat %s left row %d", seats[index].SeatLabel, seats[0].RowOrder)
		}
	}
}

// Numbered rows lift the 24-letter ceiling, which an arena needs.
func TestNumberedRowsGoPastTheAlphabet(t *testing.T) {
	if _, err := (RowSpec{Rows: 40, SeatsPerRow: 2}).Generate("sec_1"); err == nil {
		t.Fatal("forty LETTERED rows were accepted from twenty-four letters")
	}
	seats, err := RowSpec{
		Rows: 40, SeatsPerRow: 2, RowLabels: RowsNumbered,
	}.Generate("sec_stand")
	if err != nil {
		t.Fatalf("forty numbered rows: %v", err)
	}
	if len(seats) != 80 {
		t.Fatalf("forty rows of two produced %d seats", len(seats))
	}
	if seats[len(seats)-1].RowLabel != "40" {
		t.Errorf("the last row is %q, want \"40\"", seats[len(seats)-1].RowLabel)
	}
}

func TestNumberedRowsCanStartPartWayUp(t *testing.T) {
	seats, err := RowSpec{
		Rows: 3, SeatsPerRow: 1, RowLabels: RowsNumbered, FirstRowLetter: "13",
	}.Generate("sec_upper")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	for index, want := range []string{"13", "14", "15"} {
		if seats[index].RowLabel != want {
			t.Errorf("row %d is %q, want %q", index, seats[index].RowLabel, want)
		}
	}
}

func TestAnArcNeedsARadius(t *testing.T) {
	// Below the first row is the arena floor, which is not seating. A radius of
	// zero would put a row of chairs where the bulls are.
	if _, err := (RowSpec{Shape: ShapeArc, Rows: 1, SeatsPerRow: 4}).Generate("sec"); err == nil {
		t.Fatal("an arc with no radius was accepted")
	}
}

func TestAnArcRefusesAnImpossibleSweep(t *testing.T) {
	for _, sweep := range []float64{-10, 400} {
		_, err := RowSpec{
			Shape: ShapeArc, Rows: 1, SeatsPerRow: 4, Radius: 50, SweepAngle: sweep,
		}.Generate("sec")
		if err == nil {
			t.Errorf("a sweep of %.0f degrees was accepted", sweep)
		}
	}
}

// Round tables: the other rounded venue, and a different one.
func TestTablesRingTheirSeatsAndReadAsRows(t *testing.T) {
	seats, err := TableSpec{
		Tables: 3, SeatsPerTable: 8, TableRadius: 30, PerRow: 2, Gap: 100,
	}.Generate("sec_camarote")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if len(seats) != 24 {
		t.Fatalf("three tables of eight produced %d seats", len(seats))
	}

	// A table is a ROW, which is what makes "four seats together" mean "four
	// seats at one table" with no new concept anywhere downstream.
	tables := map[string]int{}
	for _, seat := range seats {
		tables[seat.RowLabel]++
	}
	for _, table := range []string{"1", "2", "3"} {
		if tables[table] != 8 {
			t.Errorf("table %s has %d seats, want 8", table, tables[table])
		}
	}

	// Every seat of a table is the same distance from that table's centre.
	byTable := map[string][]Seat{}
	for _, seat := range seats {
		byTable[seat.RowLabel] = append(byTable[seat.RowLabel], seat)
	}
	for table, ring := range byTable {
		centreX, centreY := 0.0, 0.0
		for _, seat := range ring {
			centreX += seat.X
			centreY += seat.Y
		}
		centreX /= float64(len(ring))
		centreY /= float64(len(ring))
		for _, seat := range ring {
			got := math.Hypot(seat.X-centreX, seat.Y-centreY)
			if math.Abs(got-30) > 0.001 {
				t.Errorf("table %s seat %s sits %.3f from its centre, want 30",
					table, seat.SeatLabel, got)
			}
		}
	}
}

func TestTablesGridWrapsAtPerRow(t *testing.T) {
	seats, err := TableSpec{
		Tables: 4, SeatsPerTable: 4, TableRadius: 10, PerRow: 2, Gap: 100,
	}.Generate("sec")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	centre := func(table string) (float64, float64) {
		x, y, n := 0.0, 0.0, 0.0
		for _, seat := range seats {
			if seat.RowLabel == table {
				x += seat.X
				y += seat.Y
				n++
			}
		}
		return x / n, y / n
	}
	_, y1 := centre("1")
	_, y3 := centre("3")
	// Two per row, so table 3 starts the second rank and sits further down.
	if y3 <= y1 {
		t.Errorf("table 3 is at y=%.1f and table 1 at y=%.1f; the grid did not wrap", y3, y1)
	}
}

func TestTablesCanContinueAnotherFloorsNumbering(t *testing.T) {
	seats, err := TableSpec{
		Tables: 2, SeatsPerTable: 2, FirstTable: 21,
	}.Generate("sec")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if seats[0].RowLabel != "21" {
		t.Errorf("the first table is %q, want \"21\"", seats[0].RowLabel)
	}
}

func TestATableSectionNeedsATable(t *testing.T) {
	if _, err := (TableSpec{SeatsPerTable: 8}).Generate("sec"); err == nil {
		t.Fatal("a section with no tables was accepted")
	}
}
