package seating

import "fmt"

// Two things cannot occupy the same floor.
//
// A room is a physical place. Two sectors sharing the same square metres is not
// an unusual layout, it is a mistake — and a silent one, because the seats
// underneath still generate, still materialise and still sell. The buyer finds
// out when two people arrive at the same chair from different sectors.
//
// The check lives here rather than in the editor because the editor is one
// client. A layout is saved through an HTTP endpoint somebody can call directly,
// and "the canvas would not let you do that" is not a rule, it is a habit.
//
// WHAT A SECTION OCCUPIES is the whole difficulty, and a bounding box is the
// wrong answer. A ring of stands wrapped around an arena is a rodeo — the most
// ordinary round venue there is — and its bounding box swallows the arena
// whole. So a block of chairs occupies where its CHAIRS are, and scenery
// occupies its rectangle, and a collision is the two genuinely meeting.

// ErrSectionsOverlap means two parts of the room are drawn on top of each
// other. It names both, because "something overlaps" sends the organiser
// looking through a room they thought was fine.
type ErrSectionsOverlap struct {
	First  string
	Second string
}

func (e ErrSectionsOverlap) Error() string {
	return fmt.Sprintf("seating: %q and %q are drawn on top of each other", e.First, e.Second)
}

// Box is a rectangle in layout units.
type Box struct {
	MinX, MinY, MaxX, MaxY float64
}

// Empty reports whether this box encloses nothing, which is what an unplaced or
// sizeless section produces. Nothing can collide with nothing.
func (b Box) Empty() bool { return b.MaxX <= b.MinX || b.MaxY <= b.MinY }

func (b Box) grown(by float64) Box {
	return Box{MinX: b.MinX - by, MinY: b.MinY - by, MaxX: b.MaxX + by, MaxY: b.MaxY + by}
}

func (b Box) holds(x, y float64) bool {
	return x >= b.MinX && x <= b.MaxX && y >= b.MinY && y <= b.MaxY
}

// holdsRound is the same question for an ELLIPSE inscribed in the box.
//
// An arena is drawn as a circle, so its floor is a circle, and its bounding box
// is not it. The corners of that box reach a full 41% further from the centre
// than the edge does — enough that an organiser who sizes the disc to sit
// neatly inside a ring of stands would be told it collides with them, at four
// corners that are not on screen. That is the original arena complaint in a new
// disguise.
func (b Box) holdsRound(x, y float64) bool {
	radiusX := (b.MaxX - b.MinX) / 2
	radiusY := (b.MaxY - b.MinY) / 2
	if radiusX <= 0 || radiusY <= 0 {
		return false
	}
	dx := (x - (b.MinX + radiusX)) / radiusX
	dy := (y - (b.MinY + radiusY)) / radiusY
	return dx*dx+dy*dy <= 1
}

// Point is where one chair is.
type Point struct{ X, Y float64 }

// Footprint is what one part of the room physically occupies.
//
// Exactly one of the two is set. A marker or a counted area is a rectangle it
// declared; a block of named chairs is the chairs themselves, and never their
// bounding box — see the package comment above for the rodeo that proves it.
type Footprint struct {
	ID   string
	Name string
	Box  Box
	// Round says the box describes a CIRCLE inscribed in it, which is how an
	// arena is drawn and therefore what its floor is.
	Round bool
	Seats []Point
}

const (
	// chair is how much room one seat takes around its own centre. Seats within
	// a row sit 24 units apart by default, so two chairs from different sectors
	// closer together than this are the same piece of floor.
	chair = 12.0
	// touching is how much two rectangles may share before it counts. Not zero:
	// adjacent sectors share an edge — a balcony directly behind the stalls,
	// two stands meeting at a corner — and calling that a collision would
	// refuse almost every real venue.
	touching = 1.0
)

// Overlaps reports the first pair of footprints that genuinely meet.
//
// Ordered by the slice, so the pair reported is stable for a given room: an
// error that names a different sector each time it is raised reads as two
// different problems.
func Overlaps(footprints []Footprint) error {
	// One grid per seated block, built once. A rodeo is fifteen hundred chairs
	// and comparing every pair of two blocks directly is the one shape of this
	// problem that gets slower exactly on the rooms that have the most seats.
	grids := make([]*grid, len(footprints))
	for index := range footprints {
		if len(footprints[index].Seats) > 0 {
			grids[index] = newGrid(footprints[index].Seats)
		}
	}

	for i := range footprints {
		for j := i + 1; j < len(footprints); j++ {
			if meet(footprints[i], footprints[j], grids[i], grids[j]) {
				return ErrSectionsOverlap{
					First:  named(footprints[i].Name),
					Second: named(footprints[j].Name),
				}
			}
		}
	}
	return nil
}

// Colliding reports every footprint that is on top of another, by id.
//
// The editor paints all of them at once: an organiser who has nudged one block
// into two others wants to see both problems, not to fix one and discover the
// next. The save reports one pair, because an error message is a sentence.
func Colliding(footprints []Footprint) []string {
	grids := make([]*grid, len(footprints))
	for index := range footprints {
		if len(footprints[index].Seats) > 0 {
			grids[index] = newGrid(footprints[index].Seats)
		}
	}

	hit := make(map[string]bool, len(footprints))
	for i := range footprints {
		for j := i + 1; j < len(footprints); j++ {
			if meet(footprints[i], footprints[j], grids[i], grids[j]) {
				hit[footprints[i].ID] = true
				hit[footprints[j].ID] = true
			}
		}
	}
	// In the order they were given, so the answer is stable between two
	// identical rooms.
	out := make([]string, 0, len(hit))
	for index := range footprints {
		if hit[footprints[index].ID] {
			out = append(out, footprints[index].ID)
		}
	}
	return out
}

// meet reports whether two footprints occupy any of the same floor.
func meet(a, b Footprint, gridA, gridB *grid) bool {
	switch {
	case len(a.Seats) > 0 && len(b.Seats) > 0:
		// Chairs against chairs. The smaller block is walked and the larger one
		// is the one indexed, so the cost is the smaller count either way.
		if len(a.Seats) > len(b.Seats) {
			a, b = b, a
			gridA, gridB = gridB, gridA
		}
		for _, seat := range a.Seats {
			if gridB.occupied(seat) {
				return true
			}
		}
		return false

	case len(a.Seats) > 0:
		// Chairs against a rectangle. This is the case that has to let a ring
		// of stands surround an arena: no chair is INSIDE the arena, so nothing
		// meets, however much the block's bounding box would have overlapped.
		return anySeatIn(b, a.Seats)

	case len(b.Seats) > 0:
		return anySeatIn(a, b.Seats)

	default:
		// Two shapes with no chairs, compared as rectangles even when one is
		// round. Deliberately conservative: a stage tucked into the corner of an
		// arena's bounding box is flagged, which is stricter than the geometry
		// needs and much simpler than ellipse-to-rectangle intersection — and
		// two pieces of scenery overlapping at all is almost always the mistake
		// it looks like.
		if a.Box.Empty() || b.Box.Empty() {
			return false
		}
		shareX := min(a.Box.MaxX, b.Box.MaxX) - max(a.Box.MinX, b.Box.MinX)
		shareY := min(a.Box.MaxY, b.Box.MaxY) - max(a.Box.MinY, b.Box.MinY)
		// BOTH dimensions have to genuinely overlap. Two sectors side by side
		// share a full span of one axis and none of the other, and that is a
		// room rather than a collision.
		return shareX > touching && shareY > touching
	}
}

func anySeatIn(shape Footprint, seats []Point) bool {
	if shape.Box.Empty() {
		return false
	}
	// Grown by a chair's own room: a seat whose centre sits just outside the
	// stage is still a seat with a stage through it.
	grown := shape.Box.grown(chair - touching)
	for _, seat := range seats {
		inside := grown.holds(seat.X, seat.Y)
		if shape.Round {
			inside = grown.holdsRound(seat.X, seat.Y)
		}
		if inside {
			return true
		}
	}
	return false
}

func named(name string) string {
	if name == "" {
		return "setor sem nome"
	}
	return name
}

// grid buckets chairs by position so "is anything within a chair of here" is a
// handful of map lookups rather than a scan.
type grid struct {
	cells map[[2]int][]Point
}

func newGrid(seats []Point) *grid {
	g := &grid{cells: make(map[[2]int][]Point, len(seats))}
	for _, seat := range seats {
		key := [2]int{int(seat.X / chair), int(seat.Y / chair)}
		g.cells[key] = append(g.cells[key], seat)
	}
	return g
}

func (g *grid) occupied(at Point) bool {
	cellX, cellY := int(at.X/chair), int(at.Y/chair)
	// The nine cells around it, because a chair's reach crosses a cell edge.
	for dx := -1; dx <= 1; dx++ {
		for dy := -1; dy <= 1; dy++ {
			for _, seat := range g.cells[[2]int{cellX + dx, cellY + dy}] {
				if (seat.X-at.X)*(seat.X-at.X)+(seat.Y-at.Y)*(seat.Y-at.Y) < chair*chair {
					return true
				}
			}
		}
	}
	return false
}

// FootprintOf is what one section occupies, ready for Overlaps.
func FootprintOf(section Section, seats []Seat) Footprint {
	if section.Kind.Seated() {
		points := make([]Point, 0, len(seats))
		for index := range seats {
			points = append(points, Point{X: seats[index].X, Y: seats[index].Y})
		}
		return Footprint{ID: section.ID, Name: section.Name, Seats: points}
	}
	return Footprint{
		ID:   section.ID,
		Name: section.Name,
		// An arena is a circle on screen, so it is a circle here.
		Round: section.Kind == SectionArena,
		Box: Box{
			MinX: section.OffsetX,
			MinY: section.OffsetY,
			MaxX: section.OffsetX + section.Width,
			MaxY: section.OffsetY + section.Height,
		},
	}
}
