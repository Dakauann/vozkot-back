package seating

import (
	"fmt"
	"strings"
	"time"
)

// Venue is the physical room, owned by an organiser and reused across events.
//
// It exists so a theatre draws its room ONCE and sells two hundred nights from
// it. Events keep their own free-text venue and address fields for the party in
// a warehouse that has no plan and never will; a Venue is the opt-in upgrade
// for a room with fixed chairs.
type Venue struct {
	ID      string
	OwnerID string
	Name    string
	// Capacity is the sum of the published layout's seats and standing room. A
	// cached number: the layout is the authority, and this is what a listing
	// shows without counting seats.
	Capacity  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Layout is one arrangement of one venue.
//
// A room has more than one. A theatre sells "configuração padrão" most nights
// and "configuração acústica" with the stage pushed forward for a handful, and
// the two have different seats in different places. Both belong to the same
// venue.
type Layout struct {
	ID      string
	VenueID string
	OwnerID string
	Name    string
	// Version rises on every copy-on-write. An event binds to a VERSION, not
	// to a layout, so editing the room next season cannot rewrite what last
	// season sold.
	Version int
	Status  LayoutStatus
	// Frozen is set the moment any event bound to this version sells a seat.
	// After that, editing copies to Version+1 rather than changing this one.
	//
	// It is belt to the braces of the snapshot columns on the sold seat: the
	// snapshot means a sold ticket keeps its name even if this fails, and this
	// means it should never have to.
	Frozen bool
	// ViewBox is the coordinate space seats are placed in, as width and height.
	// Unitless on purpose: the client scales it to whatever screen it has.
	ViewBoxWidth  int
	ViewBoxHeight int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Where the stage is, and what sits in the middle, used to live here as a
// three-valued Focus on the layout. It does not any more: a stage and an arena
// are SECTIONS, with a position and a size, because that is what they are.
//
// As an enum the stage had one derived position, could not be moved, and a room
// could have a stage or an arena and never both -- which a rodeo with a show
// stage at one end has. See SectionStage and SectionArena.

type LayoutStatus string

const (
	LayoutDraft     LayoutStatus = "draft"
	LayoutPublished LayoutStatus = "published"
	LayoutArchived  LayoutStatus = "archived"
)

// Sellable reports whether an event may bind to this layout.
func (s LayoutStatus) Sellable() bool { return s == LayoutPublished }

// Section is a named block of a layout: Plateia A, Balcão, Camarote 3.
type Section struct {
	ID       string
	LayoutID string
	Name     string
	Kind     SectionKind
	// Capacity is how many people a standing or booth section admits. Zero and
	// meaningless for a seated section, where the seats are the capacity.
	Capacity int
	// OffsetX and OffsetY are where this object sits: its TOP-LEFT corner.
	//
	// The same corner for every shape, and that uniformity is load-bearing. The
	// generators do not agree on an origin — a block of rows starts at one, an
	// arc of stands surrounds it — so a canvas placing a box from an offset
	// would have to know the shape first, and dragging two blocks the same
	// distance would move them by different amounts. See PlaceAt.
	OffsetX float64
	OffsetY float64
	// Width and Height size a MARKER — a stage, an arena. Meaningless for a
	// block of seats, whose size is its seats.
	//
	// Stored rather than derived. The arena's radius used to be computed from
	// the nearest seat, which meant it changed every time a seat moved and grew
	// to swallow the room on a partial stand.
	Width  float64
	Height float64
	// Shape is the polygon the client paints, as flattened x,y pairs in the
	// layout's coordinate space. Stored, never interpreted here.
	Shape []float64
	// Definition is the form that generated this block, stored verbatim and
	// never interpreted here.
	//
	// Without it a saved room can be looked at and not edited: the seats are an
	// output, and many different forms produce the same coordinates, so nothing
	// can recover "ten rows of sixteen, aisle after six, odd/even from the
	// centre" from a field of dots. Opaque on purpose — it is the editor's
	// format, and this package has no business having an opinion about it.
	Definition []byte
	// DisplayOrder is the order sections are listed in to a buyer, which is the
	// organiser's editorial choice and not alphabetical.
	DisplayOrder int
}

// Seat is one chair in a layout: the definition, not the thing on sale.
//
// What is on sale is an EventSeat, which is this seat bound to one night with a
// price and a status. A Seat is reused by every event the layout ever hosts.
type Seat struct {
	ID        string
	SectionID string
	RowLabel  string
	SeatLabel string
	// X and Y place the seat in the layout's coordinate space.
	X float64
	Y float64
	// Rotation orients a seat in a curved row, in degrees.
	Rotation float64
	Kind     SeatKind
	// RowOrder and SeatOrder are the authoritative ordering.
	//
	// The labels cannot be sorted: "A" through "P" skips I in most houses, seat
	// "10" sorts before seat "9" as text, and older theatres number odd seats
	// one way from the centre aisle and even seats the other. Adjacency — which
	// is what "four seats together" means — is defined by these integers and
	// never by the labels.
	RowOrder  int
	SeatOrder int
}

// Validate checks a seat definition before it is stored.
func (s *Seat) Validate() error {
	if strings.TrimSpace(s.SectionID) == "" {
		return ErrInvalidSection
	}
	if strings.TrimSpace(s.RowLabel) == "" || strings.TrimSpace(s.SeatLabel) == "" {
		return ErrInvalidLabel
	}
	if s.Kind == "" {
		s.Kind = SeatStandard
	}
	if !s.Kind.valid() {
		return ErrInvalidSeatKind
	}
	return nil
}

// Validate checks a section definition before it is stored.
func (s *Section) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return ErrInvalidLabel
	}
	if s.Kind == "" {
		s.Kind = SectionSeated
	}
	if !s.Kind.valid() {
		return ErrInvalidSectionKnd
	}
	if s.Kind.Marker() && (s.Width <= 0 || s.Height <= 0) {
		// A marker with no size draws nothing and cannot be grabbed, which is
		// how a stage becomes impossible to move.
		return fmt.Errorf("seating: a %s needs a width and a height", s.Kind)
	}
	return nil
}
