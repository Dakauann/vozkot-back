package seating

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	domain "vozkot/domain/seating"
)

// The builder's model, against a real PostgreSQL.
//
// What is proved here is the thing the previous version could not express: a
// room is a list of OBJECTS, each with a place and a size of its own, and the
// scenery is one of them.

// Every block keeps its own place.
//
// The old room generated every section at the same origin, so a stage, a
// plateia and two VIP wings came out as one pile of dots on top of each other —
// which is why no arrangement more complicated than "one block of rows" could
// be built at all.
func TestEachBlockKeepsItsOwnPlace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	sections, seats, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{
		{
			Name: "Palco", Kind: domain.SectionStage,
			OffsetX: 300, OffsetY: 0, Width: 300, Height: 60,
		},
		{
			Name: "Plateia", Kind: domain.SectionSeated,
			OffsetX: 320, OffsetY: 120,
			Rows: domain.RowSpec{Rows: 4, SeatsPerRow: 8},
		},
		{
			Name: "VIP Esquerda", Kind: domain.SectionSeated,
			OffsetX: 60, OffsetY: 120,
			Rows: domain.RowSpec{Rows: 4, SeatsPerRow: 3},
		},
		{
			Name: "VIP Direita", Kind: domain.SectionSeated,
			OffsetX: 700, OffsetY: 120,
			Rows: domain.RowSpec{Rows: 4, SeatsPerRow: 3},
		},
	})
	if err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}
	if len(sections) != 4 {
		t.Fatalf("got %d sections, want 4", len(sections))
	}

	// Every block starts exactly where it was placed. PlaceAt settles the
	// generators' disagreement about their own origin, so this holds for a
	// straight plateia and for a ring of stands alike.
	for _, section := range sections {
		if section.Kind.Marker() {
			continue
		}
		minX, minY := math.Inf(1), math.Inf(1)
		for index := range seats {
			if seats[index].SectionID != section.ID {
				continue
			}
			minX = math.Min(minX, seats[index].X)
			minY = math.Min(minY, seats[index].Y)
		}
		if math.Abs(minX-section.OffsetX) > 0.001 || math.Abs(minY-section.OffsetY) > 0.001 {
			t.Errorf("%s starts at (%.1f, %.1f), was placed at (%.1f, %.1f)",
				section.Name, minX, minY, section.OffsetX, section.OffsetY)
		}
	}

	// And no two blocks occupy the same point, which is the bug this proves
	// gone rather than merely asserting the offsets came back.
	seen := map[string]string{}
	for index := range seats {
		key := fmt.Sprintf("%.2f,%.2f", seats[index].X, seats[index].Y)
		if other, clash := seen[key]; clash && other != seats[index].SectionID {
			t.Fatalf("two sections share the point %s", key)
		}
		seen[key] = seats[index].SectionID
	}
}

// A stage is a section with a size, and both survive a round trip.
//
// It used to be a word on the layout with a position derived from the seats, so
// nothing could move it, nothing could resize it, and a room could have a stage
// or an arena and never both — which a rodeo with a show stage at one end has.
func TestMarkersAreStoredAsPlacedObjects(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	if _, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{
		{
			Name: "Arena", Kind: domain.SectionArena,
			OffsetX: 100, OffsetY: 100, Width: 280, Height: 280,
		},
		{
			Name: "Palco", Kind: domain.SectionStage,
			OffsetX: 420, OffsetY: 40, Width: 300, Height: 60,
		},
		{
			Name: "Arquibancada", Kind: domain.SectionSeated,
			OffsetX: 60, OffsetY: 60,
			Rows: domain.RowSpec{
				Shape: domain.ShapeArc, Rows: 3, SeatsPerRow: 24,
				Radius: 180, SweepAngle: 360, SeatPitch: 26,
				RowLabels: domain.RowsNumbered,
			},
		},
	}); err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}

	stored, err := h.service.Layout(ctx, h.actor, layoutID)
	if err != nil {
		t.Fatalf("Layout(): %v", err)
	}

	markers := map[string]domain.Section{}
	for _, section := range stored.Sections {
		if section.Kind.Marker() {
			markers[section.Name] = section
		}
	}
	if len(markers) != 2 {
		t.Fatalf("got %d markers, want both an arena and a stage", len(markers))
	}
	if arena := markers["Arena"]; arena.OffsetX != 100 || arena.Width != 280 || arena.Height != 280 {
		t.Errorf("arena stored as (%.0f, %.0f) %.0fx%.0f, want (100, 100) 280x280",
			arena.OffsetX, arena.OffsetY, arena.Width, arena.Height)
	}
	if stage := markers["Palco"]; stage.OffsetX != 420 || stage.Width != 300 || stage.Height != 60 {
		t.Errorf("stage stored as (%.0f, %.0f) %.0fx%.0f, want (420, 40) 300x60",
			stage.OffsetX, stage.OffsetY, stage.Width, stage.Height)
	}
	// A marker sells nothing, so it has no seats in it at all.
	for index := range stored.Seats {
		for name, marker := range markers {
			if stored.Seats[index].SectionID == marker.ID {
				t.Fatalf("marker %s has a seat in it", name)
			}
		}
	}
}

// A marker with no size draws nothing and cannot be grabbed, which is exactly
// how a stage becomes impossible to move.
func TestAMarkerNeedsASize(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	_, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{
		{Name: "Palco", Kind: domain.SectionStage, OffsetX: 10, OffsetY: 10},
	})
	if err == nil {
		t.Fatal("GenerateLayout() accepted a stage with no size")
	}
	if !strings.Contains(err.Error(), "width") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// A block keeps the editor's own description of itself, verbatim.
//
// Without it a saved room can be looked at and never edited: the seats are an
// output, and many different forms produce the same coordinates, so nothing
// could recover "four rows of eight, numbered odd and even from the centre"
// from a field of dots.
func TestABlockRemembersTheFormThatDrewIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	definition := []byte(`{"rows":4,"seatsPerRow":8,"numbering":"odd_even"}`)
	if _, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{{
		Name:       "Plateia",
		Kind:       domain.SectionSeated,
		Definition: definition,
		Rows:       domain.RowSpec{Rows: 4, SeatsPerRow: 8},
	}}); err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}

	stored, err := h.service.Layout(ctx, h.actor, layoutID)
	if err != nil {
		t.Fatalf("Layout(): %v", err)
	}
	if len(stored.Sections) != 1 {
		t.Fatalf("got %d sections, want 1", len(stored.Sections))
	}
	got := strings.ReplaceAll(string(stored.Sections[0].Definition), " ", "")
	if !strings.Contains(got, `"seatsPerRow":8`) || !strings.Contains(got, `"numbering":"odd_even"`) {
		t.Errorf("the definition came back as %q, want the form it went in as", got)
	}
}

// The buyer's map carries the scenery, so a picker can draw the room somebody is
// walking into rather than a field of dots.
func TestTheMapCarriesTheMarkers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	sections, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{
		{
			Name: "Palco", Kind: domain.SectionStage,
			OffsetX: 200, OffsetY: 0, Width: 300, Height: 60,
		},
		{
			Name: "Plateia", Kind: domain.SectionSeated, OffsetX: 200, OffsetY: 120,
			Rows: domain.RowSpec{Rows: 2, SeatsPerRow: 4},
		},
	})
	if err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}
	if err := h.service.PublishLayout(ctx, h.actor, layoutID); err != nil {
		t.Fatalf("PublishLayout(): %v", err)
	}

	var seated string
	for _, section := range sections {
		if section.Kind.Seated() {
			seated = section.ID
		}
	}
	tierID := seedTier(t, h, 8)
	if _, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID:         h.eventID,
		LayoutID:        layoutID,
		TicketBySection: map[string]string{seated: tierID},
	}); err != nil {
		t.Fatalf("Bind(): %v", err)
	}
	// Registered AFTER the tier, so it runs before it: cleanups unwind in
	// reverse, and a materialised seat holds a foreign key on the ticket it
	// sells. Dropping the tier first leaves the seats behind for every later
	// test in the suite to trip over.
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM event_seats WHERE event_id = ?", h.eventID)
		h.db.Exec("DELETE FROM event_seatings WHERE event_id = ?", h.eventID)
	})

	view, err := h.service.Map(ctx, h.eventID, 0)
	if err != nil {
		t.Fatalf("Map(): %v", err)
	}
	if len(view.Markers) != 1 {
		t.Fatalf("got %d markers on the map, want the stage", len(view.Markers))
	}
	stage := view.Markers[0]
	if stage.Kind != domain.SectionStage || stage.X != 200 || stage.Width != 300 {
		t.Errorf("the stage came back as %+v, want the one that was placed", stage)
	}

	// Not with a delta: the stage does not move between two polls, and a busy
	// onsale polls every few seconds per open picker.
	delta, err := h.service.Map(ctx, h.eventID, view.Version)
	if err != nil {
		t.Fatalf("Map(delta): %v", err)
	}
	if len(delta.Markers) != 0 {
		t.Errorf("a delta re-sent %d markers", len(delta.Markers))
	}
}

// A save with two sectors on the same floor is refused, and a preview of the
// same room is not.
//
// Both halves matter. The canvas has to be able to DRAW a collision — that is
// how the organiser sees the one they are making, mid-drag — and the store must
// never hold one, because the seats underneath generate and sell regardless of
// whether the room makes physical sense.
func TestAnOverlappingRoomIsRefusedOnSaveAndDrawnOnPreview(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	onTopOfEachOther := []SectionSpec{
		{
			Name: "Plateia", Kind: domain.SectionSeated, OffsetX: 200, OffsetY: 200,
			Rows: domain.RowSpec{Rows: 6, SeatsPerRow: 10},
		},
		{
			Name: "Palco", Kind: domain.SectionStage,
			OffsetX: 260, OffsetY: 240, Width: 300, Height: 60,
		},
	}

	_, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, onTopOfEachOther)
	var clash domain.ErrSectionsOverlap
	if !errors.As(err, &clash) {
		t.Fatalf("GenerateLayout() = %v, want an ErrSectionsOverlap", err)
	}
	if clash.First == "" || clash.Second == "" || clash.First == clash.Second {
		t.Errorf("the error named %q and %q, want both sectors", clash.First, clash.Second)
	}

	// Nothing was stored.
	stored, err := h.service.Layout(ctx, h.actor, layoutID)
	if err != nil {
		t.Fatalf("Layout(): %v", err)
	}
	if len(stored.Sections) != 0 {
		t.Errorf("the refused room left %d sections behind", len(stored.Sections))
	}

	// But it draws, so the organiser can see what they have done.
	sections, seats, err := h.service.PreviewLayout(ctx, h.actor, layoutID, onTopOfEachOther)
	if err != nil {
		t.Fatalf("PreviewLayout() refused a room the canvas has to draw: %v", err)
	}
	if len(sections) != 2 || len(seats) == 0 {
		t.Errorf("preview gave %d sections and %d seats, want the whole room",
			len(sections), len(seats))
	}
}

// The same two pieces, moved apart, save without complaint — including when
// they share an edge, which is what a balcony behind the stalls looks like.
func TestSectorsThatOnlyTouchStillSave(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	sections, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{
		{
			Name: "Palco", Kind: domain.SectionStage,
			OffsetX: 200, OffsetY: 0, Width: 300, Height: 60,
		},
		{
			Name: "Plateia", Kind: domain.SectionSeated, OffsetX: 200, OffsetY: 100,
			Rows: domain.RowSpec{Rows: 6, SeatsPerRow: 10},
		},
		{
			Name: "Camarote", Kind: domain.SectionBooth, Capacity: 6,
			OffsetX: 600, OffsetY: 100, Width: 160, Height: 110,
		},
	})
	if err != nil {
		t.Fatalf("GenerateLayout() refused a room with nothing on top of anything: %v", err)
	}
	if len(sections) != 3 {
		t.Fatalf("got %d sections, want 3", len(sections))
	}
}
