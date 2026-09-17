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
		// A rodeo: a ring of stands, the arena in the hole, and a show stage
		// clear of both. The arena is CONCENTRIC with the ring and inside its
		// first row, which is the only way the two are not on top of each other
		// — and the room refuses to save if they are.
		//
		// The ring is placed by its top-left corner, so its centre is that
		// corner plus the outer radius: 60 + (180 + 2 x 28) = 296.
		{
			Name: "Arena", Kind: domain.SectionArena,
			OffsetX: 296 - 150, OffsetY: 296 - 150, Width: 300, Height: 300,
		},
		{
			Name: "Palco", Kind: domain.SectionStage,
			OffsetX: 620, OffsetY: 620, Width: 300, Height: 60,
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
	if arena := markers["Arena"]; arena.OffsetX != 146 || arena.Width != 300 || arena.Height != 300 {
		t.Errorf("arena stored as (%.0f, %.0f) %.0fx%.0f, want (146, 146) 300x300",
			arena.OffsetX, arena.OffsetY, arena.Width, arena.Height)
	}
	if stage := markers["Palco"]; stage.OffsetX != 620 || stage.Width != 300 || stage.Height != 60 {
		t.Errorf("stage stored as (%.0f, %.0f) %.0fx%.0f, want (620, 620) 300x60",
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

	// Priced by BAND, which for a section that set none is its own name. The
	// section ids are no longer the join between geometry and money.
	var band string
	for _, section := range sections {
		if section.Kind.Seated() {
			band = section.Name
		}
	}
	if band != "Plateia" {
		t.Fatalf("the seated section is named %q, want Plateia", band)
	}
	tierID := seedTier(t, h, 8)
	if _, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID:          h.eventID,
		LayoutID:         layoutID,
		TicketByCategory: map[string]string{band: tierID},
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

// The counted floor reaches the buyer too, not only the scenery.
//
// This is the concert hall's shape: a stage, a standing pista between it and
// the chairs, and a box out on each wing. The map used to send the stage alone,
// which drew a room with holes in it — a 180 unit gap where the pista is, and
// no camarotes at all, which also put the widest things in the room outside the
// bounding box the client fits to. A buyer reading that map sees a distance the
// plan does not have, and asks why.
func TestTheMapCarriesTheCountedFloorAndNotOnlyTheScenery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	if _, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{
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
			Rows: domain.RowSpec{Rows: 2, SeatsPerRow: 4},
		},
		{
			Name: "Camarote esquerdo", Kind: domain.SectionBooth, Capacity: 10,
			OffsetX: 20, OffsetY: 320, Width: 180, Height: 116,
		},
	}); err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}
	if err := h.service.PublishLayout(ctx, h.actor, layoutID); err != nil {
		t.Fatalf("PublishLayout(): %v", err)
	}

	tierID := seedTier(t, h, 8)
	if _, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID:          h.eventID,
		LayoutID:         layoutID,
		TicketByCategory: map[string]string{"Cadeiras": tierID},
	}); err != nil {
		t.Fatalf("Bind(): %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM event_seats WHERE event_id = ?", h.eventID)
		h.db.Exec("DELETE FROM event_seatings WHERE event_id = ?", h.eventID)
	})

	view, err := h.service.Map(ctx, h.eventID, 0)
	if err != nil {
		t.Fatalf("Map(): %v", err)
	}

	byName := make(map[string]MarkerView, len(view.Markers))
	for _, marker := range view.Markers {
		byName[marker.Name] = marker
	}
	for _, name := range []string{"Palco", "Pista", "Camarote esquerdo"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("the map left out %q; it sent %v", name, byName)
		}
	}
	if _, ok := byName["Cadeiras"]; ok {
		t.Error("the seated block came through as a marker as well as chairs")
	}

	// The capacity travels, because a pista cannot be counted by eye the way a
	// block of chairs can.
	if pista := byName["Pista"]; pista.Capacity != 600 {
		t.Errorf("the pista holds %d on the map, want 600", pista.Capacity)
	}
	if stage := byName["Palco"]; stage.Capacity != 0 {
		t.Errorf("the stage reports a capacity of %d; scenery holds nobody", stage.Capacity)
	}

	// And the boxes are what the bounding box would otherwise miss: this one
	// starts at x=20, well left of the chairs at x=270.
	if box := byName["Camarote esquerdo"]; box.X != 20 || box.Width != 180 {
		t.Errorf("the camarote came back as %+v, want the one that was placed", box)
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

// THE FEATURE: the front rows of one sector cost more than the rest.
//
// This is what price bands were built for. Before them the only way to charge
// more for Fila A was to make the front rows a different SECTION — changing the
// room's geography to express a price, and splitting the meia-entrada quota in
// the process. Now one section carries two bands.
func TestFrontRowsCanCostMoreThanTheRestOfTheirSection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	// Rows A and B of a six-row plateia are priced as "Plateia Premium"; the
	// section itself is plain "Plateia".
	premium := map[string]string{}
	for _, row := range []string{"A", "B"} {
		for seat := 1; seat <= 4; seat++ {
			premium[domain.SeatKindKey(row, fmt.Sprint(seat))] = "Plateia Premium"
		}
	}

	sections, seats, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{{
		Name: "Plateia",
		Kind: domain.SectionSeated,
		Rows: domain.RowSpec{
			Rows: 6, SeatsPerRow: 4,
			CategoryByLabel: premium,
		},
	}})
	if err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}
	if len(sections) != 1 {
		t.Fatalf("got %d sections, want one — the whole point is that it is ONE sector",
			len(sections))
	}

	// Eight chairs carry the premium band and sixteen carry none, which resolves
	// to the section's own name.
	marked := 0
	for index := range seats {
		if seats[index].Category == "Plateia Premium" {
			marked++
		}
	}
	if marked != 8 {
		t.Errorf("%d chairs are premium, want 8", marked)
	}

	// The API resolves the band for every chair, so nothing downstream repeats
	// the fallback.
	stored, err := h.service.Layout(ctx, h.actor, layoutID)
	if err != nil {
		t.Fatalf("Layout(): %v", err)
	}
	bands := map[string]int{}
	for index := range stored.Seats {
		seat := &stored.Seats[index]
		bands[domain.Category(seat.Category, sections[0].Category, sections[0].Name)]++
	}
	if bands["Plateia Premium"] != 8 || bands["Plateia"] != 16 {
		t.Fatalf("bands resolved to %v, want 8 premium and 16 plateia", bands)
	}

	// And each band sells at its own price.
	cheap := seedTier(t, h, 16)
	dear := seedTier(t, h, 8)
	if err := h.service.PublishLayout(ctx, h.actor, layoutID); err != nil {
		t.Fatalf("PublishLayout(): %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM event_seats WHERE event_id = ?", h.eventID)
		h.db.Exec("DELETE FROM event_seatings WHERE event_id = ?", h.eventID)
	})
	manifest, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID:  h.eventID,
		LayoutID: layoutID,
		TicketByCategory: map[string]string{
			"Plateia":         cheap,
			"Plateia Premium": dear,
		},
	})
	if err != nil {
		t.Fatalf("Bind(): %v", err)
	}
	if manifest.SeatCount != 24 {
		t.Errorf("materialised %d seats, want all 24", manifest.SeatCount)
	}

	var dearSeats, cheapSeats int64
	h.db.Raw("SELECT count(*) FROM event_seats WHERE event_id = ? AND ticket_id = ?",
		h.eventID, dear).Scan(&dearSeats)
	h.db.Raw("SELECT count(*) FROM event_seats WHERE event_id = ? AND ticket_id = ?",
		h.eventID, cheap).Scan(&cheapSeats)
	if dearSeats != 8 || cheapSeats != 16 {
		t.Errorf("sold as %d premium and %d standard, want 8 and 16", dearSeats, cheapSeats)
	}
}

// A band left out of the price list is not sold, which is how a balcony closes
// for one night without the room being edited.
func TestAnUnpricedBandIsNotSold(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	sections, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{
		{
			Name: "Plateia", Kind: domain.SectionSeated, OffsetX: 0, OffsetY: 200,
			Rows: domain.RowSpec{Rows: 4, SeatsPerRow: 5},
		},
		{
			Name: "Balcão", Kind: domain.SectionSeated, OffsetX: 0, OffsetY: 600,
			Rows: domain.RowSpec{Rows: 2, SeatsPerRow: 5},
		},
	})
	if err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}
	if len(sections) != 2 {
		t.Fatalf("got %d sections, want 2", len(sections))
	}
	if err := h.service.PublishLayout(ctx, h.actor, layoutID); err != nil {
		t.Fatalf("PublishLayout(): %v", err)
	}

	tierID := seedTier(t, h, 20)
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM event_seats WHERE event_id = ?", h.eventID)
		h.db.Exec("DELETE FROM event_seatings WHERE event_id = ?", h.eventID)
	})
	manifest, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID:          h.eventID,
		LayoutID:         layoutID,
		TicketByCategory: map[string]string{"Plateia": tierID},
	})
	if err != nil {
		t.Fatalf("Bind(): %v", err)
	}
	if manifest.SeatCount != 20 {
		t.Errorf("materialised %d seats, want only the 20 of the priced band",
			manifest.SeatCount)
	}
}

// A price list naming a band no seat is in is refused.
//
// Silently selling nothing is the alternative, and a typo would then produce an
// event that materialised half a house and looked like it worked.
func TestAPriceListNamingAnUnknownBandIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	if _, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{{
		Name: "Plateia", Kind: domain.SectionSeated,
		Rows: domain.RowSpec{Rows: 2, SeatsPerRow: 4},
	}}); err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}
	if err := h.service.PublishLayout(ctx, h.actor, layoutID); err != nil {
		t.Fatalf("PublishLayout(): %v", err)
	}

	tierID := seedTier(t, h, 8)
	_, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID:          h.eventID,
		LayoutID:         layoutID,
		TicketByCategory: map[string]string{"Plateia Premiuim": tierID},
	})
	if err == nil {
		t.Fatal("Bind() accepted a price list for a band the room does not have")
	}
	if !strings.Contains(err.Error(), "Premiuim") {
		t.Errorf("the error does not name the band that was not found: %v", err)
	}
}

// A plan that was never published still sells, and binding publishes it.
//
// The separate publish step was the worst dead end in the product: an organiser
// drew a room, reached the pricing screen, and was told the venue had no
// published plan — about the plan they had just finished. The bind already
// freezes the layout in the transaction that writes the seats, so the freeze was
// always the protection and the publish was a second lock on a self-locking
// door. Eventbrite's reserved-seating flow has no publish step for a venue map
// at all.
func TestBindingADraftPlanPublishesAndFreezesIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	sections, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{{
		Name: "Plateia", Kind: domain.SectionSeated,
		Rows: domain.RowSpec{Rows: 3, SeatsPerRow: 6},
	}})
	if err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}

	// Never published. This is the state a plan is in the moment it is drawn.
	before, err := h.service.Layout(ctx, h.actor, layoutID)
	if err != nil {
		t.Fatalf("Layout(): %v", err)
	}
	if before.Layout.Status != domain.LayoutDraft {
		t.Fatalf("the plan starts as %q, want a draft", before.Layout.Status)
	}
	if before.Layout.Frozen {
		t.Fatal("a freshly drawn plan is already frozen")
	}

	tierID := seedTier(t, h, 18)
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM event_seats WHERE event_id = ?", h.eventID)
		h.db.Exec("DELETE FROM event_seatings WHERE event_id = ?", h.eventID)
	})
	manifest, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID:          h.eventID,
		LayoutID:         layoutID,
		TicketByCategory: map[string]string{sections[0].Name: tierID},
	})
	if err != nil {
		t.Fatalf("Bind() refused a draft plan: %v", err)
	}
	if manifest.SeatCount != 18 {
		t.Errorf("materialised %d seats, want 18", manifest.SeatCount)
	}

	after, err := h.service.Layout(ctx, h.actor, layoutID)
	if err != nil {
		t.Fatalf("Layout(): %v", err)
	}
	if after.Layout.Status != domain.LayoutPublished {
		t.Errorf("after binding the plan is %q, want published", after.Layout.Status)
	}
	if !after.Layout.Frozen {
		t.Error("after binding the plan is not frozen, so an edit could still move a sold chair")
	}
}

// An archived plan is the one refusal left: that is an organiser saying the
// room is retired.
func TestBindingAnArchivedPlanIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.emptyRoom(t)

	sections, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{{
		Name: "Plateia", Kind: domain.SectionSeated,
		Rows: domain.RowSpec{Rows: 2, SeatsPerRow: 4},
	}})
	if err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}
	h.db.Exec("UPDATE venue_layouts SET status = 'archived' WHERE id = ?", layoutID)

	tierID := seedTier(t, h, 8)
	_, err = h.service.Bind(ctx, h.actor, BindInput{
		EventID:          h.eventID,
		LayoutID:         layoutID,
		TicketByCategory: map[string]string{sections[0].Name: tierID},
	})
	if err == nil {
		t.Fatal("Bind() accepted an archived plan")
	}
}

// A listed plan carries its size.
//
// The library's whole job is telling plans apart at a glance, and a list of
// names and version numbers is the same guess the pricing screen used to ask
// for. The count comes from one grouped query for the whole list rather than a
// detail fetch per row.
func TestListedPlansCarryTheirSeatCount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	venue, err := h.service.CreateVenue(ctx, h.actor, CreateVenueInput{Name: "Teatro"})
	if err != nil {
		t.Fatalf("CreateVenue(): %v", err)
	}
	t.Cleanup(func() { h.db.Exec("DELETE FROM venues WHERE id = ?", venue.ID) })

	sizes := map[string]int{"Pequena": 24, "Grande": 160}
	for name, seats := range sizes {
		layout, err := h.service.CreateLayout(ctx, h.actor, CreateLayoutInput{
			VenueID: venue.ID, Name: name,
		})
		if err != nil {
			t.Fatalf("CreateLayout(%s): %v", name, err)
		}
		t.Cleanup(func() {
			h.db.Exec(`DELETE FROM layout_seats WHERE section_id IN
				(SELECT id FROM layout_sections WHERE layout_id = ?)`, layout.ID)
			h.db.Exec("DELETE FROM layout_sections WHERE layout_id = ?", layout.ID)
			h.db.Exec("DELETE FROM venue_layouts WHERE id = ?", layout.ID)
		})
		perRow := seats / 4
		if _, _, err := h.service.GenerateLayout(ctx, h.actor, layout.ID, []SectionSpec{{
			Name: "Plateia", Kind: domain.SectionSeated,
			Rows: domain.RowSpec{Rows: 4, SeatsPerRow: perRow},
		}}); err != nil {
			t.Fatalf("GenerateLayout(%s): %v", name, err)
		}
	}

	listed, err := h.service.ListLayouts(ctx, h.actor, venue.ID)
	if err != nil {
		t.Fatalf("ListLayouts(): %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("got %d plans, want 2", len(listed))
	}
	for _, layout := range listed {
		if layout.SeatCount != sizes[layout.Name] {
			t.Errorf("%q lists %d seats, want %d", layout.Name, layout.SeatCount, sizes[layout.Name])
		}
	}
}

// A plan with nothing drawn in it lists zero rather than nothing.
func TestAnEmptyPlanListsZeroSeats(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	venue, err := h.service.CreateVenue(ctx, h.actor, CreateVenueInput{Name: "Galpão"})
	if err != nil {
		t.Fatalf("CreateVenue(): %v", err)
	}
	layout, err := h.service.CreateLayout(ctx, h.actor, CreateLayoutInput{
		VenueID: venue.ID, Name: "Vazia",
	})
	if err != nil {
		t.Fatalf("CreateLayout(): %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM venue_layouts WHERE id = ?", layout.ID)
		h.db.Exec("DELETE FROM venues WHERE id = ?", venue.ID)
	})

	listed, err := h.service.ListLayouts(ctx, h.actor, venue.ID)
	if err != nil {
		t.Fatalf("ListLayouts(): %v", err)
	}
	if len(listed) != 1 || listed[0].SeatCount != 0 {
		t.Fatalf("got %+v, want one plan with no seats", listed)
	}
}
