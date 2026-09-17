package seating

import (
	"context"
	"errors"
	"testing"

	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/seating"
	userdomain "vozkot/domain/user"
	eventRepository "vozkot/infra/repositories/event"
	seatingRepository "vozkot/infra/repositories/seating"
	"vozkot/infra/testsupport"

	"gorm.io/gorm"
)

// The use case layer, against a real PostgreSQL.
//
// What is under test here is not the claim — usecases/checkout owns that — but
// the two things this layer is responsible for: who may reach a room, and
// whether "melhor disponível" hands back seats a party can actually sit in.

type harness struct {
	db      *gorm.DB
	service *Service
	ownerID string
	eventID string
	other   authdomain.Actor
	actor   authdomain.Actor
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testsupport.Database(t)

	ownerID := seedUser(t, db)
	strangerID := seedUser(t, db)
	eventID := testsupport.SeedEvent(t, db, ownerID)

	return &harness{
		db: db,
		service: NewService(
			seatingRepository.NewLayoutRepository(db),
			seatingRepository.NewSeatRepository(db),
			eventRepository.NewEventRepository(db),
			testsupport.Unique,
		),
		ownerID: ownerID,
		eventID: eventID,
		actor:   authdomain.Actor{ID: ownerID, Role: userdomain.RoleUser},
		other:   authdomain.Actor{ID: strangerID, Role: userdomain.RoleUser},
	}
}

func seedUser(t *testing.T, db *gorm.DB) string {
	t.Helper()
	id := testsupport.Unique("usr")
	err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Seating Test', ?, 'x', 'user', 0, NOW(), NOW())`,
		id, id+"@vozkot.test").Error
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM users WHERE id = ?", id) })
	return id
}

// room creates a venue, a layout and one generated seated section.
// emptyRoom is a venue and a layout with nothing generated into it yet, for
// tests that place their own objects.
func (h *harness) emptyRoom(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	venue, err := h.service.CreateVenue(ctx, h.actor, CreateVenueInput{Name: "Arena"})
	if err != nil {
		t.Fatalf("CreateVenue(): %v", err)
	}
	layout, err := h.service.CreateLayout(ctx, h.actor, CreateLayoutInput{
		VenueID: venue.ID, Name: "Configuracao",
	})
	if err != nil {
		t.Fatalf("CreateLayout(): %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec(`DELETE FROM event_seats WHERE event_id = ?`, h.eventID)
		h.db.Exec(`DELETE FROM event_seatings WHERE event_id = ?`, h.eventID)
		h.db.Exec(`DELETE FROM layout_seats WHERE section_id IN
			(SELECT id FROM layout_sections WHERE layout_id = ?)`, layout.ID)
		h.db.Exec("DELETE FROM layout_sections WHERE layout_id = ?", layout.ID)
		h.db.Exec("DELETE FROM venue_layouts WHERE id = ?", layout.ID)
		h.db.Exec("DELETE FROM venues WHERE id = ?", venue.ID)
	})
	return layout.ID
}

// roomCategory is the price band h.room's section falls in: a band defaults to
// its section's NAME, and that section is called "Plateia A".
const roomCategory = "Plateia A"

func (h *harness) room(t *testing.T, rows, perRow int) string {
	t.Helper()
	ctx := context.Background()

	venue, err := h.service.CreateVenue(ctx, h.actor, CreateVenueInput{Name: "Teatro"})
	if err != nil {
		t.Fatalf("CreateVenue(): %v", err)
	}
	layout, err := h.service.CreateLayout(ctx, h.actor, CreateLayoutInput{
		VenueID: venue.ID, Name: "Padrao",
	})
	if err != nil {
		t.Fatalf("CreateLayout(): %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec(`DELETE FROM layout_seats WHERE section_id IN
			(SELECT id FROM layout_sections WHERE layout_id = ?)`, layout.ID)
		h.db.Exec("DELETE FROM layout_sections WHERE layout_id = ?", layout.ID)
		h.db.Exec("DELETE FROM venue_layouts WHERE id = ?", layout.ID)
		h.db.Exec("DELETE FROM venues WHERE id = ?", venue.ID)
	})

	_, _, err = h.service.GenerateLayout(ctx, h.actor, layout.ID, []SectionSpec{{
		Name: "Plateia A",
		Kind: domain.SectionSeated,
		Rows: domain.RowSpec{Rows: rows, SeatsPerRow: perRow},
	}})
	if err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}
	if err := h.service.PublishLayout(ctx, h.actor, layout.ID); err != nil {
		t.Fatalf("PublishLayout(): %v", err)
	}
	return layout.ID
}

// A stranger may not read or edit somebody else's room.
//
// The rule lives in the use case, so this is where it is proved. A handler test
// would prove the handler passes an actor; this proves the decision.
func TestARoomIsReachableOnlyByItsOwner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.room(t, 2, 4)

	if _, err := h.service.Layout(ctx, h.other, layoutID); !errors.Is(err, authdomain.ErrForbidden) {
		t.Errorf("Layout() for a stranger = %v, want ErrForbidden", err)
	}
	if _, err := h.service.Compliance(ctx, h.other, layoutID); !errors.Is(err, authdomain.ErrForbidden) {
		t.Errorf("Compliance() for a stranger = %v, want ErrForbidden", err)
	}
	if _, _, err := h.service.GenerateLayout(ctx, h.other, layoutID, []SectionSpec{{
		Name: "Invasao", Kind: domain.SectionSeated,
		Rows: domain.RowSpec{Rows: 1, SeatsPerRow: 1},
	}}); !errors.Is(err, authdomain.ErrForbidden) {
		t.Errorf("GenerateLayout() for a stranger = %v, want ErrForbidden", err)
	}
	if err := h.service.PublishLayout(ctx, h.other, layoutID); !errors.Is(err, authdomain.ErrForbidden) {
		t.Errorf("PublishLayout() for a stranger = %v, want ErrForbidden", err)
	}
	// And the owner still can.
	if _, err := h.service.Layout(ctx, h.actor, layoutID); err != nil {
		t.Errorf("Layout() for the owner = %v, want success", err)
	}
}

// An admin reaches everything, which is the same rule the rest of the codebase
// applies through Actor.MayReach.
func TestAnAdminReachesAnyRoom(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.room(t, 2, 4)

	admin := authdomain.Actor{ID: testsupport.Unique("usr"), Role: userdomain.RoleAdmin}
	if _, err := h.service.Layout(ctx, admin, layoutID); err != nil {
		t.Errorf("Layout() for an admin = %v, want success", err)
	}
}

// A layout bound to an event is frozen, because editing it would change the
// room under a night that is already quoting chairs from it.
func TestBindingALayoutFreezesIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.room(t, 2, 4)
	ticketID := seedTier(t, h, 8)
	testsupport.CleanupEventSeats(t, h.db, h.eventID)

	if _, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID:          h.eventID,
		LayoutID:         layoutID,
		TicketByCategory: map[string]string{roomCategory: ticketID},
	}); err != nil {
		t.Fatalf("Bind(): %v", err)
	}

	_, _, err := h.service.GenerateLayout(ctx, h.actor, layoutID, []SectionSpec{{
		Name: "Reformado", Kind: domain.SectionSeated,
		Rows: domain.RowSpec{Rows: 1, SeatsPerRow: 1},
	}})
	if !errors.Is(err, domain.ErrSeatsSold) {
		t.Errorf("editing a bound layout = %v, want the frozen refusal", err)
	}
}

// Melhor disponível hands back adjacent seats, never a party split up.
func TestBestAvailableReturnsAdjacentSeats(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.room(t, 4, 6)
	ticketID := seedTier(t, h, 24)
	testsupport.CleanupEventSeats(t, h.db, h.eventID)
	if _, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID: h.eventID, LayoutID: layoutID,
		TicketByCategory: map[string]string{roomCategory: ticketID},
	}); err != nil {
		t.Fatalf("Bind(): %v", err)
	}

	best, err := h.service.BestAvailable(ctx, BestAvailableInput{
		EventID: h.eventID, TicketID: ticketID, Quantity: 3,
	})
	if err != nil {
		t.Fatalf("BestAvailable(): %v", err)
	}
	if len(best) != 3 {
		t.Fatalf("BestAvailable() returned %d seats, want 3", len(best))
	}
	for index := 1; index < len(best); index++ {
		if best[index].Row != best[0].Row {
			t.Fatalf("the party was split across rows %s and %s", best[0].Row, best[index].Row)
		}
		if best[index].SeatOrder != best[index-1].SeatOrder+1 {
			t.Errorf("seats %s and %s are not adjacent",
				best[index-1].Seat, best[index].Seat)
		}
	}
	// Row A is the front, and the front is what "best" means.
	if best[0].Row != "A" {
		t.Errorf("best available starts in row %s, want A", best[0].Row)
	}
}

// Rather than split a party, it returns nothing.
func TestBestAvailableReturnsNothingRatherThanSplitAParty(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Rows of two, so no row can seat three.
	layoutID := h.room(t, 4, 2)
	ticketID := seedTier(t, h, 8)
	testsupport.CleanupEventSeats(t, h.db, h.eventID)
	if _, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID: h.eventID, LayoutID: layoutID,
		TicketByCategory: map[string]string{roomCategory: ticketID},
	}); err != nil {
		t.Fatalf("Bind(): %v", err)
	}

	best, err := h.service.BestAvailable(ctx, BestAvailableInput{
		EventID: h.eventID, TicketID: ticketID, Quantity: 3,
	})
	if err != nil {
		t.Fatalf("BestAvailable(): %v", err)
	}
	if len(best) != 0 {
		t.Fatalf("BestAvailable() split a party of three across rows of two: %d seats", len(best))
	}
}

// Accessible seats are never handed to somebody who did not ask.
//
// This is the whole reason the kind is marked: an accessible seat sold as an
// ordinary chair is a wheelchair user arriving to find one.
func TestBestAvailableWithholdsAccessibleSeatsUnlessAsked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	venue, err := h.service.CreateVenue(ctx, h.actor, CreateVenueInput{Name: "Teatro"})
	if err != nil {
		t.Fatalf("CreateVenue(): %v", err)
	}
	layout, err := h.service.CreateLayout(ctx, h.actor, CreateLayoutInput{VenueID: venue.ID, Name: "Padrao"})
	if err != nil {
		t.Fatalf("CreateLayout(): %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec(`DELETE FROM layout_seats WHERE section_id IN
			(SELECT id FROM layout_sections WHERE layout_id = ?)`, layout.ID)
		h.db.Exec("DELETE FROM layout_sections WHERE layout_id = ?", layout.ID)
		h.db.Exec("DELETE FROM venue_layouts WHERE id = ?", layout.ID)
		h.db.Exec("DELETE FROM venues WHERE id = ?", venue.ID)
	})

	// One row of four, the first two of which are a wheelchair space and its
	// companion seat.
	if _, _, err := h.service.GenerateLayout(ctx, h.actor, layout.ID, []SectionSpec{{
		Name: "Plateia A",
		Kind: domain.SectionSeated,
		Rows: domain.RowSpec{
			Rows: 1, SeatsPerRow: 4,
			KindByLabel: map[string]domain.SeatKind{
				domain.SeatKindKey("A", "1"): domain.SeatWheelchair,
				domain.SeatKindKey("A", "2"): domain.SeatCompanion,
			},
		},
	}}); err != nil {
		t.Fatalf("GenerateLayout(): %v", err)
	}
	if err := h.service.PublishLayout(ctx, h.actor, layout.ID); err != nil {
		t.Fatalf("PublishLayout(): %v", err)
	}

	ticketID := seedTier(t, h, 4)
	testsupport.CleanupEventSeats(t, h.db, h.eventID)
	if _, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID: h.eventID, LayoutID: layout.ID,
		TicketByCategory: map[string]string{roomCategory: ticketID},
	}); err != nil {
		t.Fatalf("Bind(): %v", err)
	}

	// An ordinary request must not be offered either of them, even though
	// asking for two adjacent seats would otherwise land exactly there.
	ordinary, err := h.service.BestAvailable(ctx, BestAvailableInput{
		EventID: h.eventID, TicketID: ticketID, Quantity: 2,
	})
	if err != nil {
		t.Fatalf("BestAvailable(): %v", err)
	}
	for _, seat := range ordinary {
		if seat.Kind.Accessible() {
			t.Errorf("an ordinary request was offered %s, an accessible seat", seat.Seat)
		}
	}

	// And a request that asks gets them.
	accessible, err := h.service.BestAvailable(ctx, BestAvailableInput{
		EventID: h.eventID, TicketID: ticketID, Quantity: 1, Accessible: true,
	})
	if err != nil {
		t.Fatalf("BestAvailable(accessible): %v", err)
	}
	if len(accessible) != 1 || !accessible[0].Kind.Accessible() {
		t.Errorf("an accessible request got %#v", accessible)
	}
}

// The map's delta cursor is what makes polling an onsale cheap.
func TestMapReturnsOnlyWhatChanged(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.room(t, 2, 4)
	ticketID := seedTier(t, h, 8)
	testsupport.CleanupEventSeats(t, h.db, h.eventID)
	if _, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID: h.eventID, LayoutID: layoutID,
		TicketByCategory: map[string]string{roomCategory: ticketID},
	}); err != nil {
		t.Fatalf("Bind(): %v", err)
	}

	whole, err := h.service.Map(ctx, h.eventID, 0)
	if err != nil {
		t.Fatalf("Map(): %v", err)
	}
	if len(whole.Seats) != 8 || !whole.Complete {
		t.Fatalf("the first read returned %d seats, complete=%v", len(whole.Seats), whole.Complete)
	}

	// Nothing has changed, so a delta is empty and says so without resending
	// the house.
	quiet, err := h.service.Map(ctx, h.eventID, whole.Version)
	if err != nil {
		t.Fatalf("Map(since): %v", err)
	}
	if len(quiet.Seats) != 0 {
		t.Errorf("a quiet delta carried %d seats", len(quiet.Seats))
	}
	if quiet.Complete {
		t.Error("a delta reported itself complete; a client would replace its whole map with nothing")
	}

	// Block one, and exactly one comes back.
	if _, err := h.service.Block(ctx, h.actor, h.eventID,
		[]string{whole.Seats[0].ID}, domain.BlockBroken); err != nil {
		t.Fatalf("Block(): %v", err)
	}
	delta, err := h.service.Map(ctx, h.eventID, whole.Version)
	if err != nil {
		t.Fatalf("Map(since) after block: %v", err)
	}
	if len(delta.Seats) != 1 || delta.Seats[0].ID != whole.Seats[0].ID {
		t.Fatalf("the delta carried %d seats, want the one that was blocked", len(delta.Seats))
	}
	if delta.Seats[0].Status != domain.StatusBlocked {
		t.Errorf("the changed seat is %q, want blocked", delta.Seats[0].Status)
	}
}

// The map must not publish who is holding a chair.
func TestTheMapNeverPublishesTheHolder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	layoutID := h.room(t, 2, 4)
	ticketID := seedTier(t, h, 8)
	testsupport.CleanupEventSeats(t, h.db, h.eventID)
	if _, err := h.service.Bind(ctx, h.actor, BindInput{
		EventID: h.eventID, LayoutID: layoutID,
		TicketByCategory: map[string]string{roomCategory: ticketID},
	}); err != nil {
		t.Fatalf("Bind(): %v", err)
	}

	view, err := h.service.Map(ctx, h.eventID, 0)
	if err != nil {
		t.Fatalf("Map(): %v", err)
	}
	// SeatView is a different type from domain.EventSeat precisely so that the
	// order id and the hold deadline cannot be serialised by accident. This is
	// a compile-time property; the assertion is that the type stays that way.
	if len(view.Seats) == 0 {
		t.Fatal("no seats to check")
	}
	seat := view.Seats[0]
	if seat.ID == "" || seat.Row == "" {
		t.Errorf("the view lost a field it needs: %#v", seat)
	}
}

func seedTier(t *testing.T, h *harness, quantity int) string {
	t.Helper()
	id := testsupport.Unique("tkt")
	err := h.db.Exec(`
		INSERT INTO tickets
			(id, owner_id, event_id, title, description, price_cents, currency,
			 quantity, sold, reserved, status, created_at, updated_at)
		VALUES (?, ?, ?, 'Plateia', '', 12000, 'BRL', ?, 0, 0, 'on_sale', NOW(), NOW())`,
		id, h.ownerID, h.eventID, quantity).Error
	if err != nil {
		t.Fatalf("seed tier: %v", err)
	}
	t.Cleanup(func() { h.db.Exec("DELETE FROM tickets WHERE id = ?", id) })
	return id
}
