package seating_test

import (
	"errors"
	"math"
	"testing"

	seating "vozkot/domain/seating"
)

func rect(id string, minX, minY, maxX, maxY float64) seating.Footprint {
	return seating.Footprint{
		ID:   id,
		Name: id,
		Box:  seating.Box{MinX: minX, MinY: minY, MaxX: maxX, MaxY: maxY},
	}
}

// disc is what FootprintOf produces for an arena: the circle inscribed in the
// box, because that is what the canvas draws.
func disc(id string, centreX, centreY, radius float64) seating.Footprint {
	shape := rect(id, centreX-radius, centreY-radius, centreX+radius, centreY+radius)
	shape.Round = true
	return shape
}

// chairs lays a grid of seats 24 units apart, the generator's own spacing.
func chairs(id string, atX, atY float64, rows, perRow int) seating.Footprint {
	points := make([]seating.Point, 0, rows*perRow)
	for row := 0; row < rows; row++ {
		for seat := 0; seat < perRow; seat++ {
			points = append(points, seating.Point{
				X: atX + float64(seat)*24,
				Y: atY + float64(row)*28,
			})
		}
	}
	return seating.Footprint{ID: id, Name: id, Seats: points}
}

// Two sectors on the same floor is a mistake, and a silent one: the seats
// underneath still generate, still materialise and still sell, and the buyer
// finds out when two people arrive at the same chair.
func TestOverlapsFindsTwoBlocksOnTheSameFloor(t *testing.T) {
	err := seating.Overlaps([]seating.Footprint{
		chairs("Plateia", 0, 0, 6, 10),
		chairs("Balcão", 48, 56, 6, 10),
	})

	var clash seating.ErrSectionsOverlap
	if !errors.As(err, &clash) {
		t.Fatalf("Overlaps() = %v, want an ErrSectionsOverlap", err)
	}
	// It names BOTH, because "something overlaps" sends an organiser hunting
	// through a room they thought was fine.
	if clash.First != "Plateia" || clash.Second != "Balcão" {
		t.Errorf("named %q and %q, want Plateia and Balcão", clash.First, clash.Second)
	}
}

// THE RODEO. A ring of stands wrapped around an arena is the most ordinary
// round venue there is, and its bounding box swallows the arena whole, which
// is exactly why a footprint is not a bounding box. No chair is inside the
// arena, so nothing meets.
func TestARingOfStandsMaySurroundAnArena(t *testing.T) {
	// Stands on a circle of radius 180 about (400, 400).
	stand := seating.Footprint{ID: "Arquibancada", Name: "Arquibancada"}
	for degrees := 0; degrees < 360; degrees += 6 {
		for _, radius := range []float64{180, 206, 232} {
			radians := float64(degrees) * math.Pi / 180
			stand.Seats = append(stand.Seats, seating.Point{
				X: 400 + radius*math.Sin(radians),
				Y: 400 - radius*math.Cos(radians),
			})
		}
	}
	// The arena fills the hole, inside the first row of stands.
	arena := disc("Arena", 400, 400, 150)

	if err := seating.Overlaps([]seating.Footprint{stand, arena}); err != nil {
		t.Fatalf("a ring of stands around an arena was refused: %v", err)
	}

	// But a stage dropped ON the stands is caught, in the same room.
	stage := rect("Palco", 400, 180, 700, 240)
	if err := seating.Overlaps([]seating.Footprint{stand, arena, stage}); err == nil {
		t.Fatal("a stage sitting on the stands was allowed")
	}

	// And a SQUARE arena of the same half-width would not fit, because its
	// corners reach 41% further out than a disc's edge, which is exactly why
	// the arena's footprint is round.
	square := rect("Arena quadrada", 400-150, 400-150, 400+150, 400+150)
	if err := seating.Overlaps([]seating.Footprint{stand, square}); err == nil {
		t.Error("a square arena whose corners cross the stands was allowed")
	}
}

// A shared edge is a balcony directly behind the stalls, or two stands meeting
// at a corner. A check that called that a collision would refuse almost every
// real venue.
func TestOverlapsAllowsSectorsThatOnlyTouch(t *testing.T) {
	cases := map[string][]seating.Footprint{
		"edge to edge across": {
			rect("A", 0, 0, 200, 100), rect("B", 200, 0, 400, 100),
		},
		"edge to edge down": {
			rect("A", 0, 0, 200, 100), rect("B", 0, 100, 200, 200),
		},
		"corner to corner": {
			rect("A", 0, 0, 200, 100), rect("B", 200, 100, 400, 200),
		},
		"two columns side by side, sharing every bit of Y": {
			rect("A", 0, 0, 100, 600), rect("B", 100, 0, 200, 600),
		},
		"two blocks of chairs in their own halves": {
			chairs("A", 0, 0, 8, 10), chairs("B", 400, 0, 8, 10),
		},
		"a stage in front of the front row": {
			rect("Palco", 0, 0, 300, 60), chairs("Plateia", 0, 100, 8, 12),
		},
	}
	for name, footprints := range cases {
		if err := seating.Overlaps(footprints); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A section with no footprint collides with nothing. A seated block whose spec
// produced no seats, or a marker with no size, is not in the way of anything.
func TestOverlapsIgnoresSectionsWithNoFootprint(t *testing.T) {
	err := seating.Overlaps([]seating.Footprint{
		{ID: "Vazio", Name: "Vazio"},
		chairs("Plateia", 0, 0, 4, 6),
		rect("Sem tamanho", 10, 10, 10, 10),
	})
	if err != nil {
		t.Fatalf("Overlaps() = %v, want nil", err)
	}
}

// A chair whose centre sits just outside a stage still has a stage through it.
func TestAChairTouchingAMarkerCounts(t *testing.T) {
	err := seating.Overlaps([]seating.Footprint{
		rect("Palco", 0, 0, 300, 60),
		// Four units below the stage's edge: less than a chair's own room.
		{ID: "Plateia", Name: "Plateia", Seats: []seating.Point{{X: 150, Y: 64}}},
	})
	if err == nil {
		t.Fatal("a chair overlapping the stage's edge was allowed")
	}
}

// Colliding names every part involved, so the editor can paint them all at
// once rather than making the organiser fix one and discover the next.
func TestCollidingNamesEveryPartInvolved(t *testing.T) {
	ids := seating.Colliding([]seating.Footprint{
		rect("middle", 100, 100, 300, 300),
		rect("left", 50, 150, 150, 250),
		rect("right", 250, 150, 350, 250),
		rect("away", 600, 600, 700, 700),
	})
	want := []string{"middle", "left", "right"}
	if len(ids) != len(want) {
		t.Fatalf("Colliding() = %v, want %v", ids, want)
	}
	for _, id := range want {
		found := false
		for _, got := range ids {
			if got == id {
				found = true
			}
		}
		if !found {
			t.Errorf("Colliding() = %v, missing %q", ids, id)
		}
	}
}

func TestCollidingIsEmptyForACleanRoom(t *testing.T) {
	if ids := seating.Colliding([]seating.Footprint{
		rect("Palco", 200, 0, 500, 60),
		chairs("Plateia", 200, 100, 8, 12),
	}); len(ids) != 0 {
		t.Errorf("Colliding() = %v, want nothing", ids)
	}
}

func TestFootprintOfUsesChairsForABlockAndSizeForAMarker(t *testing.T) {
	block := seating.FootprintOf(
		seating.Section{ID: "sec_a", Name: "Plateia", Kind: seating.SectionSeated,
			OffsetX: 999, OffsetY: 999},
		[]seating.Seat{{X: 100, Y: 200}, {X: 124, Y: 200}},
	)
	if len(block.Seats) != 2 || !block.Box.Empty() {
		t.Errorf("a seated block came back as %+v, want its chairs and no box", block)
	}

	stage := seating.FootprintOf(seating.Section{
		ID: "sec_b", Name: "Palco", Kind: seating.SectionStage,
		OffsetX: 300, OffsetY: 40, Width: 300, Height: 60,
	}, nil)
	want := seating.Box{MinX: 300, MinY: 40, MaxX: 600, MaxY: 100}
	if stage.Box != want || len(stage.Seats) != 0 {
		t.Errorf("a stage came back as %+v, want the box %+v", stage, want)
	}
}

// The band order is the colour, so it has to be stable.
//
// Slot one is the first hue of a fixed palette, slot two the second. A band that
// changed slots when another was added would repaint a room somebody had just
// learned to read, so the order is first appearance walking the sections as they
// are displayed and then their chairs as an usher reads them.
func TestBandsOfIsOrderedByFirstAppearance(t *testing.T) {
	sections := []seating.Section{
		{ID: "b", Name: "Balcão", Kind: seating.SectionSeated, DisplayOrder: 2},
		{ID: "p", Name: "Plateia", Kind: seating.SectionSeated, DisplayOrder: 1},
		{ID: "s", Name: "Palco", Kind: seating.SectionStage, DisplayOrder: 0},
	}
	seats := []seating.Seat{
		{SectionID: "p", RowOrder: 2, SeatOrder: 1},
		{SectionID: "p", RowOrder: 1, SeatOrder: 1, Category: "Plateia Premium"},
		{SectionID: "b", RowOrder: 1, SeatOrder: 1},
	}

	bands := seating.BandsOf(sections, seats)
	want := []string{"Plateia", "Plateia Premium", "Balcão"}
	if len(bands) != len(want) {
		t.Fatalf("BandsOf() = %v, want %v", bands, want)
	}
	for index := range want {
		if bands[index] != want[index] {
			t.Fatalf("BandsOf() = %v, want %v", bands, want)
		}
	}
}

// Scenery sells nothing, so it is not a price band.
func TestBandsOfSkipsScenery(t *testing.T) {
	bands := seating.BandsOf([]seating.Section{
		{ID: "s", Name: "Palco", Kind: seating.SectionStage, DisplayOrder: 0},
		{ID: "a", Name: "Arena", Kind: seating.SectionArena, DisplayOrder: 1},
		{ID: "p", Name: "Pista", Kind: seating.SectionStanding, DisplayOrder: 2},
	}, nil)
	if len(bands) != 1 || bands[0] != "Pista" {
		t.Errorf("BandsOf() = %v, want just the Pista", bands)
	}
}

// Two sections sharing a band are one band, which is the whole point of a band
// being a name.
func TestBandsOfMergesSectionsThatShareABand(t *testing.T) {
	bands := seating.BandsOf([]seating.Section{
		{ID: "l", Name: "Plateia Esquerda", Category: "Plateia", Kind: seating.SectionSeated, DisplayOrder: 1},
		{ID: "r", Name: "Plateia Direita", Category: "Plateia", Kind: seating.SectionSeated, DisplayOrder: 2},
	}, nil)
	if len(bands) != 1 || bands[0] != "Plateia" {
		t.Errorf("BandsOf() = %v, want one Plateia", bands)
	}
}
