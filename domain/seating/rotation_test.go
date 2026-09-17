package seating_test

import (
	"math"
	"testing"

	seating "vozkot/domain/seating"
)

// A quarter turn puts the rows down the side of the room.
//
// The one arrangement dragging and resizing could not reach, and the reason
// rotation exists: a block of rows runs along x, and VIP wings down the sides of
// a room need one running along y.
func TestAQuarterTurnMakesRowsRunDownTheSide(t *testing.T) {
	spec := seating.RowSpec{Rows: 3, SeatsPerRow: 8, Shape: seating.ShapeLinear}
	flat, err := spec.Generate("sec")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	turned, err := spec.Generate("sec")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	flatWidth, flatHeight := span(flat)
	if flatWidth <= flatHeight {
		t.Fatalf("a flat block is %.0f x %.0f, expected it wider than deep", flatWidth, flatHeight)
	}

	seating.RotateBy(turned, 90)
	turnedWidth, turnedHeight := span(turned)
	if math.Abs(turnedWidth-flatHeight) > 0.001 || math.Abs(turnedHeight-flatWidth) > 0.001 {
		t.Errorf("after a quarter turn the block is %.0f x %.0f, want %.0f x %.0f",
			turnedWidth, turnedHeight, flatHeight, flatWidth)
	}
}

// Rotation moves coordinates and nothing else.
//
// RowOrder and SeatOrder are the ONLY definition of adjacency in this system, so
// a turned block still seats four people together — and the labels a ticket
// prints are untouched, because a chair does not get renamed by being moved.
func TestRotationLeavesAdjacencyAndLabelsAlone(t *testing.T) {
	spec := seating.RowSpec{Rows: 4, SeatsPerRow: 6, Shape: seating.ShapeLinear}
	before, err := spec.Generate("sec")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	after, err := spec.Generate("sec")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	seating.RotateBy(after, 37)

	if len(before) != len(after) {
		t.Fatalf("rotation changed the seat count from %d to %d", len(before), len(after))
	}
	for index := range before {
		if after[index].RowLabel != before[index].RowLabel ||
			after[index].SeatLabel != before[index].SeatLabel {
			t.Fatalf("seat %d was renamed from %s/%s to %s/%s", index,
				before[index].RowLabel, before[index].SeatLabel,
				after[index].RowLabel, after[index].SeatLabel)
		}
		if after[index].RowOrder != before[index].RowOrder ||
			after[index].SeatOrder != before[index].SeatOrder {
			t.Fatalf("seat %d had its ordering changed, which is adjacency", index)
		}
	}
}

// Neighbours stay neighbours: the distance between two seats of a row is the
// same after a turn as before it, because a rotation is rigid.
func TestRotationPreservesTheSpacing(t *testing.T) {
	spec := seating.RowSpec{Rows: 2, SeatsPerRow: 5, Shape: seating.ShapeLinear}
	before, _ := spec.Generate("sec")
	after, _ := spec.Generate("sec")
	seating.RotateBy(after, 90)

	gapBefore := math.Hypot(before[1].X-before[0].X, before[1].Y-before[0].Y)
	gapAfter := math.Hypot(after[1].X-after[0].X, after[1].Y-after[0].Y)
	if math.Abs(gapBefore-gapAfter) > 0.001 {
		t.Errorf("two neighbours sat %.2f apart and now sit %.2f apart", gapBefore, gapAfter)
	}
}

// A full turn, or no turn, costs nothing and changes nothing.
func TestRotationByAFullTurnIsANoOp(t *testing.T) {
	spec := seating.RowSpec{Rows: 2, SeatsPerRow: 4, Shape: seating.ShapeLinear}
	before, _ := spec.Generate("sec")
	after, _ := spec.Generate("sec")
	seating.RotateBy(after, 360)

	for index := range before {
		if after[index].X != before[index].X || after[index].Y != before[index].Y {
			t.Fatalf("seat %d moved on a full turn", index)
		}
	}
	seating.RotateBy(nil, 90)
}

func span(seats []seating.Seat) (float64, float64) {
	minX, minY := seats[0].X, seats[0].Y
	maxX, maxY := seats[0].X, seats[0].Y
	for index := range seats {
		minX = math.Min(minX, seats[index].X)
		minY = math.Min(minY, seats[index].Y)
		maxX = math.Max(maxX, seats[index].X)
		maxY = math.Max(maxY, seats[index].Y)
	}
	return maxX - minX, maxY - minY
}
