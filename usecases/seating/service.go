// Package seating is the use cases of reserved seating: an organiser drawing a
// room, and a buyer reading the map of one.
//
// Authorization lives HERE, never in a handler. Every method takes an
// auth.Actor and decides for itself, behind one owned() helper, which is the
// same discipline usecases/ticket and usecases/event follow. A handler that
// checked ownership would be a second place for the rule to live and a second
// place for it to be forgotten.
package seating

import (
	"context"
	"fmt"
	"strings"

	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	domain "vozkot/domain/seating"
)

// MaxSeatsPerRequest bounds a generated block.
//
// A layout is written by one organiser in an editor, so this is not a hot path
// — but it is an unauthenticated-shaped request that materialises rows, and an
// unbounded one lets a single call write a million of them. Twelve thousand is
// larger than any Brazilian theatre and smaller than a denial of service.
const MaxSeatsPerRequest = 12_000

type Service struct {
	layouts domain.LayoutRepository
	seats   domain.Repository
	events  eventdomain.Repository
	newID   func(prefix string) string
}

func NewService(
	layouts domain.LayoutRepository,
	seats domain.Repository,
	events eventdomain.Repository,
	newID func(prefix string) string,
) *Service {
	return &Service{layouts: layouts, seats: seats, events: events, newID: newID}
}

// ownedVenue is the one place a venue's ownership is decided.
func (s *Service) ownedVenue(ctx context.Context, actor authdomain.Actor, id string) (*domain.Venue, error) {
	venue, err := s.layouts.VenueByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !actor.MayReach(venue.OwnerID) {
		return nil, fmt.Errorf("%w: this venue belongs to another operator", authdomain.ErrForbidden)
	}
	return venue, nil
}

// ownedLayout is the one place a layout's ownership is decided.
func (s *Service) ownedLayout(ctx context.Context, actor authdomain.Actor, id string) (*domain.Layout, error) {
	layout, err := s.layouts.LayoutByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !actor.MayReach(layout.OwnerID) {
		return nil, fmt.Errorf("%w: this layout belongs to another operator", authdomain.ErrForbidden)
	}
	return layout, nil
}

// ownedEvent is the one place an event's ownership is decided here.
//
// It defers to the event aggregate's own AuthorizeReach, which is what the
// report, the refund and the door already use: one rule about who may reach an
// event, not four.
func (s *Service) ownedEvent(ctx context.Context, actor authdomain.Actor, id string) (*eventdomain.Event, error) {
	item, err := s.events.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := item.AuthorizeReach(actor); err != nil {
		return nil, err
	}
	return item, nil
}

// --- the organiser's side ----------------------------------------------------

type CreateVenueInput struct {
	Name string
}

func (s *Service) CreateVenue(ctx context.Context, actor authdomain.Actor, input CreateVenueInput) (*domain.Venue, error) {
	if !actor.Authenticated() {
		return nil, authdomain.ErrUnauthorized
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, domain.ErrInvalidLabel
	}
	venue := &domain.Venue{ID: s.newID("ven"), OwnerID: actor.ID, Name: name}
	if err := s.layouts.CreateVenue(ctx, venue); err != nil {
		return nil, err
	}
	return venue, nil
}

func (s *Service) ListVenues(ctx context.Context, actor authdomain.Actor, limit, offset int) ([]domain.Venue, int64, error) {
	if !actor.Authenticated() {
		return nil, 0, authdomain.ErrUnauthorized
	}
	// Scoped to the actor unless they are an admin, which is the same shape
	// every other listing in this codebase uses. The alternative is what
	// produced the "601 lotes" report: a listing that trusted its caller.
	owner := actor.ID
	if actor.IsAdmin() {
		owner = ""
	}
	return s.layouts.ListVenues(ctx, owner, limit, offset)
}

type CreateLayoutInput struct {
	VenueID       string
	Name          string
	ViewBoxWidth  int
	ViewBoxHeight int
}

func (s *Service) CreateLayout(ctx context.Context, actor authdomain.Actor, input CreateLayoutInput) (*domain.Layout, error) {
	venue, err := s.ownedVenue(ctx, actor, input.VenueID)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, domain.ErrInvalidLabel
	}
	layout := &domain.Layout{
		ID:            s.newID("lay"),
		VenueID:       venue.ID,
		OwnerID:       venue.OwnerID,
		Name:          name,
		Version:       1,
		Status:        domain.LayoutDraft,
		ViewBoxWidth:  input.ViewBoxWidth,
		ViewBoxHeight: input.ViewBoxHeight,
	}
	if err := s.layouts.CreateLayout(ctx, layout); err != nil {
		return nil, err
	}
	return layout, nil
}

func (s *Service) ListLayouts(ctx context.Context, actor authdomain.Actor, venueID string) ([]domain.Layout, error) {
	if _, err := s.ownedVenue(ctx, actor, venueID); err != nil {
		return nil, err
	}
	return s.layouts.ListLayouts(ctx, venueID)
}

// SectionSpec is one block an organiser asks the generator for.
type SectionSpec struct {
	Name     string
	Kind     domain.SectionKind
	Capacity int
	// OffsetX and OffsetY are where the block sits; Width and Height size a
	// marker. The canvas sets all four by dragging.
	OffsetX      float64
	OffsetY      float64
	Width        float64
	Height       float64
	DisplayOrder int
	Shape        []float64
	// Rotation turns the block, in degrees clockwise about its own centre.
	Rotation float64
	// Category is the price band this section's seats belong to by default.
	// Empty means the section's own name.
	Category string
	// Definition is the editor's own description of this block, passed through
	// to storage untouched so the editor can reopen it. This package does not
	// read it.
	Definition []byte
	// Rows generates the block's seats. Ignored for a standing or booth
	// section, which has none: a Pista is a counter and a camarote is one unit.
	Rows domain.RowSpec
	// Tables generates round tables instead of rows, when it names any.
	//
	// The two are alternatives rather than a mode flag: a section is described
	// by rows or by tables, and asking for both is a caller that has not
	// decided. Tables win when present, because naming them is the more
	// specific statement.
	Tables domain.TableSpec
}

// GenerateLayout replaces a layout's sections and seats from row specs.
//
// The whole layout at once, not one section at a time, because the editor works
// that way: an organiser describes the room, sees it, and adjusts. A partial
// write would leave a room half-described and no way to reason about it.
func (s *Service) GenerateLayout(
	ctx context.Context,
	actor authdomain.Actor,
	layoutID string,
	specs []SectionSpec,
) ([]domain.Section, []domain.Seat, error) {
	layout, err := s.ownedLayout(ctx, actor, layoutID)
	if err != nil {
		return nil, nil, err
	}
	if layout.Frozen {
		// A frozen layout has an event bound to it. Editing it would change the
		// room under a night that is already quoting seats from it.
		return nil, nil, fmt.Errorf("seating: layout %s is in use by an event: %w",
			layoutID, domain.ErrSeatsSold)
	}
	sections, seats, err := s.buildSections(layoutID, specs)
	if err != nil {
		return nil, nil, err
	}
	// Refused on the SAVE and not on the preview.
	//
	// The preview has to be able to draw a collision — that is how the organiser
	// sees the one they are making, mid-drag, and a canvas that went blank
	// whenever two things touched would be unusable. What must not happen is a
	// room being STORED with two sectors on the same floor, because the seats
	// underneath still generate, still materialise and still sell.
	if err := domain.Overlaps(footprints(sections, seats)); err != nil {
		return nil, nil, err
	}

	if err := s.layouts.ReplaceSections(ctx, layoutID, sections, seats); err != nil {
		return nil, nil, err
	}
	return sections, seats, nil
}

// PreviewLayout builds a room and stores nothing.
//
// The editor draws the room as the organiser types, and this is what it draws.
// It runs the SAME generator the real thing runs, which is the entire point:
// re-implementing row lettering, aisle skipping, odd/even numbering and arc
// bearings in the browser would be two sources of truth about what a room
// looks like, drifting apart from the first edit, and the preview's whole job
// is to be trustworthy.
//
// Ownership is still checked. It writes nothing, so the risk is compute rather
// than data — but an unauthenticated generator that will lay out twelve
// thousand seats on request is a denial of service with a polite name.
//
// A FROZEN layout previews happily. Looking is not editing, and an organiser
// whose room is in use still wants to see what a change would do before
// deciding to make a new version.
func (s *Service) PreviewLayout(
	ctx context.Context,
	actor authdomain.Actor,
	layoutID string,
	specs []SectionSpec,
) ([]domain.Section, []domain.Seat, error) {
	if _, err := s.ownedLayout(ctx, actor, layoutID); err != nil {
		return nil, nil, err
	}
	return s.buildSections(layoutID, specs)
}

// PreviewCompliance reports the accessibility quotas against a DRAFT.
//
// The studio used to show the quotas of the SAVED room beside a canvas showing
// the draft, which produced "against 0 places, the layout meets the quotas"
// next to a sector of 192 seats. The number has to describe the room on
// screen, or it is worse than absent: an organiser reads it as a clearance.
func (s *Service) PreviewCompliance(specs []SectionSpec, seats []domain.Seat) domain.Compliance {
	standing := 0
	for index := range specs {
		spec := &specs[index]
		if !spec.Kind.Seated() && spec.Kind != "" {
			standing += spec.Capacity
		}
	}
	return domain.CheckCompliance(seats, standing)
}

// SuggestAccessibleSeats names the chairs that would satisfy the quotas.
//
// The other half of "mark the seats in the section form": the studio had no
// control that could mark anything, so the instruction was impossible to
// follow. This answers "which ones" so the button that applies it can exist.
func (s *Service) SuggestAccessibleSeats(
	seats []domain.Seat,
	report domain.Compliance,
) map[string]domain.SeatKind {
	return domain.SuggestAccessibleSeats(seats, report)
}

// buildSections turns the editor's specs into sections and seats.
//
// Shared by GenerateLayout, which persists the result, and PreviewLayout, which
// does not. One builder rather than two, so what the organiser sees on the
// canvas is what the save will store — a preview built by a second code path is
// a preview that can lie.
func (s *Service) buildSections(
	layoutID string,
	specs []SectionSpec,
) ([]domain.Section, []domain.Seat, error) {
	if len(specs) == 0 {
		return nil, nil, domain.ErrInvalidSection
	}

	sections := make([]domain.Section, 0, len(specs))
	seats := make([]domain.Seat, 0, 256)
	for index := range specs {
		spec := &specs[index]
		section := domain.Section{
			ID:           s.newID("sec"),
			LayoutID:     layoutID,
			Name:         strings.TrimSpace(spec.Name),
			Kind:         spec.Kind,
			Capacity:     spec.Capacity,
			OffsetX:      spec.OffsetX,
			OffsetY:      spec.OffsetY,
			Width:        spec.Width,
			Height:       spec.Height,
			Rotation:     spec.Rotation,
			Category:     spec.Category,
			Shape:        spec.Shape,
			Definition:   spec.Definition,
			DisplayOrder: spec.DisplayOrder,
		}
		if err := section.Validate(); err != nil {
			return nil, nil, err
		}
		sections = append(sections, section)

		if !section.Kind.Seated() {
			continue
		}
		generated, err := generateSeats(spec, section.ID)
		if err != nil {
			return nil, nil, err
		}
		for at := range generated {
			generated[at].ID = s.newID("lst")
		}
		seats = append(seats, generated...)
		if len(seats) > MaxSeatsPerRequest {
			return nil, nil, fmt.Errorf(
				"seating: a layout may define at most %d seats in one request", MaxSeatsPerRequest)
		}
	}
	return sections, seats, nil
}

// generateSeats lays out one section, by tables or by rows.
//
// One place decides which, so the HTTP layer does not have to carry a mode and
// the two specs cannot both be honoured by accident.
func generateSeats(spec *SectionSpec, sectionID string) ([]domain.Seat, error) {
	var seats []domain.Seat
	var err error
	if spec.Tables.Tables > 0 {
		seats, err = spec.Tables.Generate(sectionID)
	} else {
		seats, err = spec.Rows.Generate(sectionID)
	}
	if err != nil {
		return nil, err
	}
	// Turned first, then placed. Rotating about the block's own centre and
	// THEN setting its corner down means the two compose without the rotation
	// dragging the block across the room as a side effect.
	domain.RotateBy(seats, spec.Rotation)
	// The SECTION owns where the block sits, and it is applied here rather than
	// inside either generator: two places holding a position is one place too
	// many, and the section is the one a canvas drags.
	domain.PlaceAt(seats, spec.OffsetX, spec.OffsetY)
	return seats, nil
}

// footprints is what each section occupies, in one pass over the seats.
//
// Grouping once matters: a block's footprint is its CHAIRS, and scanning the
// whole seat list per section would be quadratic on exactly the rooms that have
// the most of them.
// Collisions reports which sections of a drawn room are on top of each other.
//
// The same rule the save refuses with, exported so the editor can paint it. The
// alternative was a second copy of the rule in the browser, and two answers to
// "is this room physically possible" disagree the moment either one is touched.
func (s *Service) Collisions(sections []domain.Section, seats []domain.Seat) []string {
	return domain.Colliding(footprints(sections, seats))
}

func footprints(sections []domain.Section, seats []domain.Seat) []domain.Footprint {
	bySection := make(map[string][]domain.Seat, len(sections))
	for index := range seats {
		id := seats[index].SectionID
		bySection[id] = append(bySection[id], seats[index])
	}
	out := make([]domain.Footprint, 0, len(sections))
	for _, section := range sections {
		out = append(out, domain.FootprintOf(section, bySection[section.ID]))
	}
	return out
}

// Compliance reports a layout against the accessibility decree.
func (s *Service) Compliance(ctx context.Context, actor authdomain.Actor, layoutID string) (domain.Compliance, error) {
	if _, err := s.ownedLayout(ctx, actor, layoutID); err != nil {
		return domain.Compliance{}, err
	}
	seats, err := s.layouts.SeatsOf(ctx, layoutID)
	if err != nil {
		return domain.Compliance{}, err
	}
	sections, err := s.layouts.SectionsOf(ctx, layoutID)
	if err != nil {
		return domain.Compliance{}, err
	}
	standing := 0
	for _, section := range sections {
		if !section.Kind.Seated() {
			standing += section.Capacity
		}
	}
	return domain.CheckCompliance(seats, standing), nil
}

// PublishLayout makes a layout bindable to an event.
func (s *Service) PublishLayout(ctx context.Context, actor authdomain.Actor, layoutID string) error {
	if _, err := s.ownedLayout(ctx, actor, layoutID); err != nil {
		return err
	}
	seats, err := s.layouts.SeatsOf(ctx, layoutID)
	if err != nil {
		return err
	}
	sections, err := s.layouts.SectionsOf(ctx, layoutID)
	if err != nil {
		return err
	}
	if len(seats) == 0 && len(sections) == 0 {
		return fmt.Errorf("seating: layout %s has nothing in it to sell", layoutID)
	}
	return s.layouts.PublishLayout(ctx, layoutID)
}

// Layout is a layout with everything needed to draw it.
type Layout struct {
	Layout   domain.Layout
	Sections []domain.Section
	Seats    []domain.Seat
}

func (s *Service) Layout(ctx context.Context, actor authdomain.Actor, layoutID string) (*Layout, error) {
	layout, err := s.ownedLayout(ctx, actor, layoutID)
	if err != nil {
		return nil, err
	}
	sections, err := s.layouts.SectionsOf(ctx, layoutID)
	if err != nil {
		return nil, err
	}
	seats, err := s.layouts.SeatsOf(ctx, layoutID)
	if err != nil {
		return nil, err
	}
	return &Layout{Layout: *layout, Sections: sections, Seats: seats}, nil
}

// BindInput puts a layout on sale for one event, at one price per section.
type BindInput struct {
	EventID  string
	LayoutID string
	// TicketByCategory is the join between geometry and money: a price band
	// mapped to the tier it sells at. A band left out is not sold, which is how
	// an organiser closes the balcony for one night.
	TicketByCategory map[string]string
}

func (s *Service) Bind(ctx context.Context, actor authdomain.Actor, input BindInput) (domain.EventSeating, error) {
	if _, err := s.ownedEvent(ctx, actor, input.EventID); err != nil {
		return domain.EventSeating{}, err
	}
	if _, err := s.ownedLayout(ctx, actor, input.LayoutID); err != nil {
		return domain.EventSeating{}, err
	}
	return s.seats.Materialise(ctx, domain.MaterialisePlan{
		EventID:          input.EventID,
		LayoutID:         input.LayoutID,
		TicketByCategory: input.TicketByCategory,
	})
}

// BindAreas gives each standing floor and box its own ticket inventory, so a
// buyer choosing one side cannot consume the other's places.
func (s *Service) BindAreas(ctx context.Context, actor authdomain.Actor, eventID string, tickets map[string]string) error {
	if _, err := s.ownedEvent(ctx, actor, eventID); err != nil {
		return err
	}
	return s.seats.BindAreas(ctx, eventID, tickets)
}

// Block and Unblock withhold an event's seats from sale.
func (s *Service) Block(ctx context.Context, actor authdomain.Actor, eventID string, seatIDs []string, reason domain.BlockReason) (int, error) {
	if _, err := s.ownedEvent(ctx, actor, eventID); err != nil {
		return 0, err
	}
	return s.seats.Block(ctx, eventID, seatIDs, reason)
}

func (s *Service) Unblock(ctx context.Context, actor authdomain.Actor, eventID string, seatIDs []string) (int, error) {
	if _, err := s.ownedEvent(ctx, actor, eventID); err != nil {
		return 0, err
	}
	return s.seats.Unblock(ctx, eventID, seatIDs)
}
