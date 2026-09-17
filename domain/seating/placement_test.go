package seating_test

import (
	"math"
	"testing"

	seating "vozkot/domain/seating"
)

// A canvas drags a box. For that to work, "where is this block" has to mean the
// same thing for every shape — and it did not: a block of rows was generated
// from its offset, an arc of stands was generated AROUND it. Dragging two
// blocks by the same distance moved them by different amounts, and no box could
// be drawn from an offset without first knowing the shape inside it.
func TestPlaceAtGivesEveryShapeTheSameCorner(t *testing.T) {
	const atX, atY float64 = 400, 250

	rows, err := seating.RowSpec{
		Rows:        4,
		SeatsPerRow: 6,
		Shape:       seating.ShapeLinear,
	}.Generate("sec_rows")
	if err != nil {
		t.Fatalf("generate rows: %v", err)
	}
	arc, err := seating.RowSpec{
		Rows:        4,
		SeatsPerRow: 20,
		Shape:       seating.ShapeArc,
		Radius:      180,
		SweepAngle:  360,
	}.Generate("sec_arc")
	if err != nil {
		t.Fatalf("generate arc: %v", err)
	}
	tables, err := seating.TableSpec{
		Tables:        4,
		SeatsPerTable: 8,
		PerRow:        2,
	}.Generate("sec_tables")
	if err != nil {
		t.Fatalf("generate tables: %v", err)
	}

	for name, seats := range map[string][]seating.Seat{
		"rows": rows, "arc": arc, "tables": tables,
	} {
		seating.PlaceAt(seats, atX, atY)
		minX, minY := math.Inf(1), math.Inf(1)
		for index := range seats {
			minX = math.Min(minX, seats[index].X)
			minY = math.Min(minY, seats[index].Y)
		}
		if math.Abs(minX-atX) > 0.001 || math.Abs(minY-atY) > 0.001 {
			t.Errorf("%s: placed at (%.1f, %.1f), want (%.1f, %.1f)",
				name, minX, minY, atX, atY)
		}
	}
}

// A shift moves the whole block and changes nothing about it. If it could
// change the spacing, a room saved after a drag would not be the room that was
// drawn, and adjacency — which is what seats a party together — is measured
// from these coordinates.
func TestPlaceAtPreservesTheShape(t *testing.T) {
	spec := seating.RowSpec{Rows: 3, SeatsPerRow: 5, Shape: seating.ShapeLinear}
	before, err := spec.Generate("sec")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	after, err := spec.Generate("sec")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	seating.PlaceAt(after, 37, -12)

	dx := after[0].X - before[0].X
	dy := after[0].Y - before[0].Y
	for index := range before {
		if math.Abs((after[index].X-before[index].X)-dx) > 0.001 ||
			math.Abs((after[index].Y-before[index].Y)-dy) > 0.001 {
			t.Fatalf("seat %d moved by a different amount than the block", index)
		}
	}
}

// Placing nothing is not an error and must not panic: a marker section has no
// seats at all, and it goes through the same funnel.
func TestPlaceAtToleratesAnEmptyBlock(t *testing.T) {
	seating.PlaceAt(nil, 10, 10)
	seating.PlaceAt([]seating.Seat{}, 10, 10)
}
