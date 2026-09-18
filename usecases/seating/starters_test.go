package seating

import (
	"context"
	"math"
	"testing"

	domain "vozkot/domain/seating"
)

// The rooms the builder offers as starting points, run through the real
// generator and the real overlap check.
//
// THE NUMBERS HERE MIRROR `starterPieces` in the frontend's builder-model.tsx,
// and that duplication is the point of the test rather than an accident of it.
// A starter that produces two sectors on the same floor makes an organiser's
// FIRST save the thing that teaches them the collision rule exists, the worst
// possible first impression, and one only the server can detect, because a
// seated block's footprint is its chairs and only the generator knows where
// those land.
//
// So if a generator default moves, the 24-unit seat gap, the 28-unit row gap,
// a table's radius, this test fails and names the starter whose geometry needs
// new numbers. Keep the two in step.
func TestEveryStarterRoomSavesWithoutCollisions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	rooms := map[string][]SectionSpec{
		"theatre": {
			{
				Name: "Palco", Kind: domain.SectionStage,
				OffsetX: 250, OffsetY: 0, Width: 300, Height: 60,
			},
			{
				Name: "Plateia", Kind: domain.SectionSeated, OffsetX: 220, OffsetY: 140,
				Rows: domain.RowSpec{Rows: 10, SeatsPerRow: 16},
			},
			{
				Name: "Balcão", Kind: domain.SectionSeated, OffsetX: 196, OffsetY: 480,
				Rows: domain.RowSpec{Rows: 4, SeatsPerRow: 18, FirstRowLetter: "L"},
			},
		},
		"show": {
			{
				Name: "Palco", Kind: domain.SectionStage,
				OffsetX: 250, OffsetY: 0, Width: 400, Height: 60,
			},
			{
				Name: "Pista", Kind: domain.SectionStanding, Capacity: 600,
				OffsetX: 250, OffsetY: 100, Width: 400, Height: 180,
			},
			{
				Name: "Cadeiras", Kind: domain.SectionSeated, OffsetX: 270, OffsetY: 320,
				Rows: domain.RowSpec{Rows: 6, SeatsPerRow: 16},
			},
			{
				Name: "Camarote esquerdo", Kind: domain.SectionBooth, Capacity: 10,
				OffsetX: 20, OffsetY: 320, Width: 180, Height: 116,
			},
			{
				Name: "Camarote direito", Kind: domain.SectionBooth, Capacity: 10,
				OffsetX: 700, OffsetY: 320, Width: 180, Height: 116,
			},
		},
		"arena": {
			{
				Name: "Arquibancada", Kind: domain.SectionSeated, OffsetX: 0, OffsetY: 0,
				Rows: domain.RowSpec{
					Shape: domain.ShapeArc, Rows: 4, SeatsPerRow: 24,
					Radius: 180, StartAngle: 0, SweepAngle: 360, SeatPitch: 26,
					RowLabels: domain.RowsNumbered,
				},
			},
			{
				Name: "Arena", Kind: domain.SectionArena,
				OffsetX: 114, OffsetY: 114, Width: 300, Height: 300,
			},
		},
		"tables": {
			{
				Name: "Palco", Kind: domain.SectionStage,
				OffsetX: 250, OffsetY: 0, Width: 300, Height: 60,
			},
			{
				Name: "Mesas", Kind: domain.SectionSeated, OffsetX: 204, OffsetY: 140,
				Tables: domain.TableSpec{Tables: 12, SeatsPerTable: 8, PerRow: 4},
			},
		},
	}

	for name, specs := range rooms {
		t.Run(name, func(t *testing.T) {
			layoutID := h.emptyRoom(t)
			sections, seats, err := h.service.GenerateLayout(ctx, h.actor, layoutID, specs)
			if err != nil {
				t.Fatalf("the %s starter does not save: %v", name, err)
			}
			if len(sections) != len(specs) {
				t.Fatalf("got %d sections, want %d", len(sections), len(specs))
			}
			// And it is a room with chairs in it, except where it is deliberately
			// all counted stock.
			if name != "show" && len(seats) == 0 {
				t.Errorf("the %s starter generated no seats", name)
			}

			// THE ROOM IS SYMMETRIC ABOUT ITS CENTRE LINE.
			//
			// The half only the eye catches. Left-aligning pieces of different
			// spans drifts their centres apart and the stage ends up off to one
			// side of the stalls it faces, which is what "some presets aren't
			// well aligned" meant.
			//
			// Not "every piece is centred", because pieces down the SIDES are
			// meant to be off-centre: that is what a side is. The rule is that
			// each one either sits on the axis or has a partner mirrored across
			// it, so a casa de show's two camarotes must be the same distance
			// out. Eight units of slack: a third of a seat pitch, enough for a
			// span that does not divide evenly and far too little to hide a
			// ragged room.
			const slack = 8.0
			boxes := map[string]domain.Box{}
			for _, footprint := range footprints(sections, seats) {
				if len(footprint.Seats) > 0 {
					boxes[footprint.Name] = seatBox(footprint.Seats)
					continue
				}
				if !footprint.Box.Empty() {
					boxes[footprint.Name] = footprint.Box
				}
			}

			room := domain.Box{MinX: math.Inf(1), MaxX: math.Inf(-1)}
			for _, box := range boxes {
				room.MinX = math.Min(room.MinX, box.MinX)
				room.MaxX = math.Max(room.MaxX, box.MaxX)
			}
			axis := (room.MinX + room.MaxX) / 2

			for name, box := range boxes {
				centre := (box.MinX + box.MaxX) / 2
				if math.Abs(centre-axis) <= slack {
					continue
				}
				mirrored := 2*axis - centre
				partnered := false
				for other, otherBox := range boxes {
					if other == name {
						continue
					}
					otherCentre := (otherBox.MinX + otherBox.MaxX) / 2
					if math.Abs(otherCentre-mirrored) <= slack {
						partnered = true
						break
					}
				}
				if !partnered {
					t.Errorf(
						"the %s starter is not symmetric: %q sits at %.0f, the room's axis is %.0f, "+
							"and nothing mirrors it at %.0f",
						name, name, centre, axis, mirrored)
				}
			}
		})
	}
}

// The theatre's lettering runs continuously across its two seated blocks.
//
// Ten rows of stalls use A to K with I skipped, so the balcony starts at L. A
// starter that restarted at A would ship every new theatre with two Fila A and
// one usher's problem.
func TestTheTheatreStarterLettersItsRowsContinuously(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	sections, seats, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{
		{
			Name: "Plateia", Kind: domain.SectionSeated, OffsetX: 200, OffsetY: 140,
			Rows: domain.RowSpec{Rows: 10, SeatsPerRow: 16},
		},
		{
			Name: "Balcão", Kind: domain.SectionSeated, OffsetX: 200, OffsetY: 480,
			Rows: domain.RowSpec{Rows: 4, SeatsPerRow: 18, FirstRowLetter: "L"},
		},
	})
	if err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}

	rows := map[string]map[string]bool{}
	byID := map[string]string{}
	for _, section := range sections {
		byID[section.ID] = section.Name
		rows[section.Name] = map[string]bool{}
	}
	for index := range seats {
		rows[byID[seats[index].SectionID]][seats[index].RowLabel] = true
	}

	if !rows["Plateia"]["K"] || rows["Plateia"]["I"] {
		t.Errorf("the stalls run %v, want A to K with I skipped", rows["Plateia"])
	}
	if !rows["Balcão"]["L"] {
		t.Errorf("the balcony runs %v, want it to start at L", rows["Balcão"])
	}
	// No letter appears in both, which is the whole point.
	for letter := range rows["Plateia"] {
		if rows["Balcão"][letter] {
			t.Errorf("row %q is in both the stalls and the balcony", letter)
		}
	}
}

// seatBox is the extent of a block's chairs, for the centring check.
func seatBox(seats []domain.Point) domain.Box {
	box := domain.Box{MinX: seats[0].X, MinY: seats[0].Y, MaxX: seats[0].X, MaxY: seats[0].Y}
	for _, seat := range seats {
		if seat.X < box.MinX {
			box.MinX = seat.X
		}
		if seat.X > box.MaxX {
			box.MaxX = seat.X
		}
		if seat.Y < box.MinY {
			box.MinY = seat.Y
		}
		if seat.Y > box.MaxY {
			box.MaxY = seat.Y
		}
	}
	return box
}
