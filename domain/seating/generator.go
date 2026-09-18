package seating

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The row generator: a form that describes a Brazilian house in one request.
//
// This exists instead of a vector editor, and that is a deliberate ordering of
// the work. Theatres and cinemas here are overwhelmingly rectangular or gently
// curved blocks of rows, so a handful of numbers describes one completely, and
// per-seat editing is only needed for the exceptions: mark this chair for a
// wheelchair, block that broken one. Building the canvas first would have been
// months spent on the part that generates the fewest seats.
//
// It is a pure function of its spec. No database, no ids from outside, nothing
// to mock: a generated room is deterministic, which is what lets the tests
// assert on labels and adjacency rather than on a fixture.

// RowLetters is the sequence house rows are lettered with.
//
// I is missing, and that is not a typo. Most Brazilian houses skip it because
// it reads as a 1 on a printed ticket and in a dark room, so a generator that
// produced a row I would produce a room that does not match the chairs bolted
// to its floor. O is skipped for the same reason against 0.
const RowLetters = "ABCDEFGHJKLMNPQRSTUVWXYZ"

// RowShape is how a block of rows is laid out in space.
//
// Two shapes cover the venues that actually exist here, and the second is not
// an embellishment: a rodeo, a stadium, a gymnasium and a circus all put the
// action in the MIDDLE and wrap the stands around it. A model that could only
// draw rows along a line could sell those venues: the claim, the adjacency
// and the labels are all shape-agnostic. But the map would not resemble the
// room, and resemblance is the entire function of a seat map. Somebody orients
// themselves by "I am behind the chutes", not by a row letter.
type RowShape string

const (
	// ShapeLinear lays rows along a line, optionally bowed toward a stage.
	// Theatres, cinemas, auditoriums.
	ShapeLinear RowShape = "linear"
	// ShapeArc lays rows as concentric arcs around a centre. Rodeo arenas,
	// stadiums, gymnasiums, arena-format shows.
	//
	// Row 0 is the CLOSEST to the centre and later rows are further out, which
	// is the direction a real stand rises in and the direction "best available"
	// already prefers.
	ShapeArc RowShape = "arc"
)

func (s RowShape) valid() bool {
	switch s {
	case ShapeLinear, ShapeArc, "":
		return true
	}
	return false
}

// RowLabelStyle is whether rows are lettered or numbered.
//
// Both exist and the choice is the venue's, not ours. A theatre says "fila K";
// an arena says "fila 23". It is also what lifts the row limit: lettering runs
// out after 24 usable letters, and an arena with forty rows is ordinary.
type RowLabelStyle string

const (
	RowsLettered RowLabelStyle = "letters"
	RowsNumbered RowLabelStyle = "numbers"
)

// Numbering is how seats within a row are numbered.
type Numbering string

const (
	// NumberingSequential is 1..n from one side. The common case.
	NumberingSequential Numbering = "sequential"
	// NumberingOddEven numbers outward from the centre aisle: odds one way,
	// evens the other. Older theatres and most of Europe.
	//
	// It matters for more than labels. A party of two in an odd/even house sits
	// in seats 5 and 7, which are ADJACENT, and a picker that reasoned about
	// adjacency from the numbers would refuse to seat them together. Adjacency
	// is why SeatOrder exists and is never derived from the label.
	NumberingOddEven Numbering = "odd_even"
)

// RowSpec generates one block of rows.
type RowSpec struct {
	// Rows is how many rows to lay out, lettered from RowLetters.
	Rows int
	// SeatsPerRow is the count in every row, before Skips.
	SeatsPerRow int
	// FirstRowLetter offsets the lettering, for a section that continues
	// another one's alphabet: a balcony that starts at N because the stalls
	// ended at M.
	FirstRowLetter string
	// Numbering is how seats are labelled within a row.
	Numbering Numbering
	// Skips are seat POSITIONS left empty: aisles, pillars, a camera platform.
	// Positions, not labels, because the label depends on the numbering.
	Skips []int
	// Shape is how the block sits in space. Empty means linear.
	Shape RowShape
	// RowLabels is whether rows are lettered or numbered. Empty means lettered,
	// which is what a theatre uses and what this generator started as.
	RowLabels RowLabelStyle
	// Curve bows a LINEAR block towards the stage, in layout units of depth at
	// the ends. Zero is a straight block. Ignored for an arc, which is already
	// curved by construction.
	Curve float64

	// --- arc only -----------------------------------------------------------
	//
	// The centre is the origin, (0,0), always. That is not a limitation: a
	// layout's coordinate space is its own, every section of a round venue
	// shares one arena, and putting the centre anywhere else would mean every
	// client had to be told where it was in order to draw the thing in the
	// middle.

	// Radius is how far the FIRST row sits from the centre. Below it is the
	// arena itself, which is not seating.
	Radius float64
	// StartAngle is where the sector begins, in degrees CLOCKWISE FROM THE TOP
	// of the map. Zero is straight up, 90 is to the right.
	//
	// Clockwise from the top because that is how a venue describes its own
	// sectors, "the stand at twelve o'clock", and because it matches screen
	// coordinates, where y grows downward, without a sign flip at the call
	// site.
	StartAngle float64
	// SweepAngle is how many degrees of the circle the sector covers. 360 is a
	// complete ring; 90 is one quadrant of stands.
	SweepAngle float64
	// SeatGap and RowGap are the spacing in layout units.
	SeatGap float64
	RowGap  float64
	// OffsetX and OffsetY place the block in the layout.
	//
	// Without them every section generated on the same spot and a room with
	// more than one block was a pile. A venue is not one block: a show has a
	// stage, a floor of chairs in front of it and VIP wings either side, and
	// the whole point of a map is that those are in different places.
	//
	// For a LINEAR block the offset moves its centre. For an ARC it moves the
	// centre of the circle, so two stands can wrap two different arenas,
	// though a single round venue leaves both at zero, which puts the arena at
	// the origin where the clients draw it.
	OffsetX float64
	OffsetY float64
	// SeatPitch is the distance between neighbouring seats along a row, and it
	// only means anything for an arc.
	//
	// When set, each row is filled at this spacing and therefore gains seats as
	// it gets further from the centre, which is what a real stand does, and
	// what makes one look like a stand rather than a fan. SeatsPerRow then
	// describes the FIRST row only.
	//
	// When zero, every row gets exactly SeatsPerRow seats, which is the simpler
	// thing to reason about and right for a shallow block.
	SeatPitch float64
	// KindByLabel overrides the kind of specific chairs, keyed "K/12".
	//
	// This is how the accessibility seats the law requires get marked without
	// a second editing pass: the generator's caller already knows which
	// positions its room reserves.
	KindByLabel map[string]SeatKind
	// CategoryByLabel puts specific chairs in a different price band, keyed the
	// same way. "A/1" through "C/16" priced as "Plateia Premium" is the front
	// rows costing more without the room being redrawn to say so.
	CategoryByLabel map[string]string
}

// PlaceAt shifts a generated block so its top-left corner sits at (x, y).
//
// The generators disagree about where a block begins: RowSpec lays a plateia out
// from its offset, generateArc wraps an arena centred on one. Both are right for
// the geometry and neither is usable as a position: "where is this block" has
// to mean one thing before anything can drag it.
//
// So placement is not a generator concern at all. A block is generated wherever
// its shape implies, and then moved as a whole.
func PlaceAt(seats []Seat, x, y float64) {
	if len(seats) == 0 {
		return
	}
	minX, minY := seats[0].X, seats[0].Y
	for index := range seats {
		if seats[index].X < minX {
			minX = seats[index].X
		}
		if seats[index].Y < minY {
			minY = seats[index].Y
		}
	}
	dx, dy := x-minX, y-minY
	if dx == 0 && dy == 0 {
		return
	}
	for index := range seats {
		seats[index].X += dx
		seats[index].Y += dy
	}
}

// RotateBy turns a generated block about the centre of its own chairs.
//
// Applied before PlaceAt, so turning and placing compose: the block spins on
// the spot and is then set down by its top-left corner like any other.
//
// It moves coordinates and NOTHING else. RowOrder and SeatOrder are the only
// definition of adjacency in this system: two seats are neighbours when they
// share a row order and their seat orders differ by one, so a block turned on
// its side still seats four people together, and the labels a ticket prints are
// untouched. The per-seat Rotation is carried along so a client can orient the
// chair it draws.
func RotateBy(seats []Seat, degrees float64) {
	if len(seats) == 0 || math.Mod(degrees, 360) == 0 {
		return
	}
	minX, minY := seats[0].X, seats[0].Y
	maxX, maxY := seats[0].X, seats[0].Y
	for index := range seats {
		minX = math.Min(minX, seats[index].X)
		minY = math.Min(minY, seats[index].Y)
		maxX = math.Max(maxX, seats[index].X)
		maxY = math.Max(maxY, seats[index].Y)
	}
	centreX, centreY := (minX+maxX)/2, (minY+maxY)/2

	radians := degrees * math.Pi / 180
	sin, cos := math.Sin(radians), math.Cos(radians)
	for index := range seats {
		dx := seats[index].X - centreX
		dy := seats[index].Y - centreY
		seats[index].X = centreX + dx*cos - dy*sin
		seats[index].Y = centreY + dx*sin + dy*cos
		seats[index].Rotation += degrees
	}
}

// SeatKindKey is the key format KindByLabel uses. One helper rather than a
// format string repeated at every caller.
func SeatKindKey(rowLabel, seatLabel string) string {
	return rowLabel + "/" + seatLabel
}

// Generate lays out a section's seats from the spec.
//
// Returns them in row-then-seat order with RowOrder and SeatOrder set, which
// together are the ONLY definition of adjacency in this system. Two seats are
// neighbours when they share a row order and their seat orders differ by one,
// whatever their labels say.
func (s RowSpec) Generate(sectionID string) ([]Seat, error) {
	if strings.TrimSpace(sectionID) == "" {
		return nil, ErrInvalidSection
	}
	if s.Rows <= 0 || s.SeatsPerRow <= 0 {
		return nil, fmt.Errorf("seating: a section needs at least one row of one seat")
	}
	if !s.Shape.valid() {
		return nil, fmt.Errorf("seating: %q is not a row shape", s.Shape)
	}

	lettered := s.RowLabels != RowsNumbered
	start := 0
	if lettered {
		// The 24-letter ceiling applies to LETTERED rows and only to them. An
		// arena with forty rows is ordinary and numbers them, which is why the
		// style is a choice rather than a limit.
		if s.Rows > len(RowLetters) {
			return nil, fmt.Errorf(
				"seating: %d rows exceeds the %d available letters; number the rows or split the block",
				s.Rows, len(RowLetters))
		}
		if letter := strings.ToUpper(strings.TrimSpace(s.FirstRowLetter)); letter != "" {
			start = strings.Index(RowLetters, letter)
			if start < 0 {
				return nil, fmt.Errorf("seating: %q is not a row letter", letter)
			}
			if start+s.Rows > len(RowLetters) {
				return nil, fmt.Errorf(
					"seating: %d rows starting at %s runs past Z", s.Rows, letter)
			}
		}
	} else if first := strings.TrimSpace(s.FirstRowLetter); first != "" {
		// The same field carries the offset for numbered rows: a stand whose
		// rows continue another's starts at 13, not at 1.
		value, err := strconv.Atoi(first)
		if err != nil || value < 1 {
			return nil, fmt.Errorf("seating: %q is not a first row number", first)
		}
		start = value - 1
	}

	if s.Shape == ShapeArc {
		return s.generateArc(sectionID, start, lettered)
	}

	seatGap := s.SeatGap
	if seatGap <= 0 {
		seatGap = 24
	}
	rowGap := s.RowGap
	if rowGap <= 0 {
		rowGap = 28
	}

	skipped := make(map[int]struct{}, len(s.Skips))
	for _, position := range s.Skips {
		skipped[position] = struct{}{}
	}

	numbering := s.Numbering
	if numbering == "" {
		numbering = NumberingSequential
	}

	width := float64(s.SeatsPerRow-1) * seatGap
	seats := make([]Seat, 0, s.Rows*s.SeatsPerRow)

	for row := 0; row < s.Rows; row++ {
		rowLabel := rowName(start+row, lettered)
		for position := 1; position <= s.SeatsPerRow; position++ {
			if _, gap := skipped[position]; gap {
				continue
			}
			seatLabel := label(numbering, position, s.SeatsPerRow)

			// Position. The curve bows the row towards the stage by pushing its
			// ends back, which is what a real raked house looks like from
			// above; a straight block is the same maths with zero arc.
			offset := float64(position-1)*seatGap - width/2
			depth := float64(row) * rowGap
			if s.Curve > 0 && width > 0 {
				// A parabola rather than a true arc: it is what the eye reads
				// as a curved row, it never doubles back on itself, and it
				// needs no trigonometry to stay monotonic.
				normalised := offset / (width / 2)
				depth -= normalised * normalised * s.Curve
			}

			kind := SeatStandard
			if override, ok := s.KindByLabel[SeatKindKey(rowLabel, seatLabel)]; ok {
				if !override.valid() {
					return nil, ErrInvalidSeatKind
				}
				kind = override
			}
			category := s.CategoryByLabel[SeatKindKey(rowLabel, seatLabel)]

			seats = append(seats, Seat{
				SectionID: sectionID,
				Category:  category,
				RowLabel:  rowLabel,
				SeatLabel: seatLabel,
				X:         s.OffsetX + offset,
				Y:         s.OffsetY + depth,
				Kind:      kind,
				RowOrder:  row,
				// The POSITION, not the label. In an odd/even house seats 5 and
				// 7 are neighbours and 5 and 6 are not, and this is the number
				// that knows it.
				SeatOrder: position,
			})
		}
	}
	if len(seats) == 0 {
		return nil, fmt.Errorf("seating: every seat in the block was skipped")
	}
	return seats, nil
}

// rowName is a row's printed name, lettered or numbered.
func rowName(index int, lettered bool) string {
	if lettered {
		return string(RowLetters[index])
	}
	return strconv.Itoa(index + 1)
}

// generateArc lays concentric rows around the origin.
//
// This is the rodeo, the stadium, the gymnasium and the circus: the action is
// in the middle and the stands wrap around it. The geometry is the only thing
// that differs from a theatre: the labels, the ordering, the adjacency, the
// accessibility kinds and every claim downstream are identical, which is why
// this is a branch in one generator rather than a second model.
func (s RowSpec) generateArc(sectionID string, start int, lettered bool) ([]Seat, error) {
	if s.Radius <= 0 {
		return nil, fmt.Errorf("seating: an arc needs a radius; below it is the arena, not seating")
	}
	sweep := s.SweepAngle
	if sweep == 0 {
		// A sector with no sweep is a line of seats stacked on one bearing,
		// which is never what anybody means. A full ring is the honest default
		// for "I did not say".
		sweep = 360
	}
	if sweep < 0 || sweep > 360 {
		return nil, fmt.Errorf("seating: a sweep of %.0f degrees is not a sector of a circle", sweep)
	}

	rowGap := s.RowGap
	if rowGap <= 0 {
		rowGap = 28
	}

	skipped := make(map[int]struct{}, len(s.Skips))
	for _, position := range s.Skips {
		skipped[position] = struct{}{}
	}
	numbering := s.Numbering
	if numbering == "" {
		numbering = NumberingSequential
	}

	// A FULL ring must not put its last seat on top of its first, so the step
	// divides the whole sweep. A partial sector should reach both of its edges,
	// so the step divides the gaps between seats. Getting this backwards is how
	// a 360 degree ring ends up with two seats in one place.
	full := sweep >= 360

	// The seat pitch, when the caller asked for one.
	//
	// A fixed count per row spreads the outer rows further and further apart,
	// because the same number of seats has more arc to cover: eight rows of
	// twenty-four at a radius of 180 puts the back row's seats twice as far
	// apart as the front row's, and the result reads as a fan of dots rather
	// than as a stand. Holding the pitch instead means a row gains seats as it
	// gets longer, which is what the chairs bolted to a real stand do.
	pitch := s.SeatPitch
	if pitch <= 0 && s.SeatsPerRow > 1 {
		// Not requested: keep the count fixed, which is the simpler behaviour
		// and right for a shallow block.
		pitch = 0
	}

	seats := make([]Seat, 0, s.Rows*s.SeatsPerRow)
	for row := 0; row < s.Rows; row++ {
		radius := s.Radius + float64(row)*rowGap
		rowLabel := rowName(start+row, lettered)

		// How many seats this row holds, and how far apart in degrees.
		perRow := s.SeatsPerRow
		if pitch > 0 {
			// Arc length at this radius, divided by the pitch.
			arc := radius * sweep * math.Pi / 180
			perRow = int(arc/pitch) + 1
			if full {
				perRow = int(arc / pitch)
			}
			if perRow < 1 {
				perRow = 1
			}
		}
		divisor := float64(perRow)
		if !full && perRow > 1 {
			divisor = float64(perRow - 1)
		}

		for position := 1; position <= perRow; position++ {
			if _, gap := skipped[position]; gap {
				continue
			}
			degrees := s.StartAngle
			if divisor > 0 {
				degrees += sweep * float64(position-1) / divisor
			}
			radians := degrees * math.Pi / 180
			seatLabel := label(numbering, position, perRow)

			kind := SeatStandard
			if override, ok := s.KindByLabel[SeatKindKey(rowLabel, seatLabel)]; ok {
				if !override.valid() {
					return nil, ErrInvalidSeatKind
				}
				kind = override
			}
			category := s.CategoryByLabel[SeatKindKey(rowLabel, seatLabel)]

			seats = append(seats, Seat{
				SectionID: sectionID,
				Category:  category,
				RowLabel:  rowLabel,
				SeatLabel: seatLabel,
				// Clockwise from the top, in screen coordinates where y grows
				// downward: x = r·sin θ puts 90 degrees to the right, and
				// y = −r·cos θ puts 0 degrees at the top. The offset moves the
				// centre of the circle, so a layout can hold more than one.
				X: s.OffsetX + radius*math.Sin(radians),
				Y: s.OffsetY - radius*math.Cos(radians),
				// Facing the middle, which is where the bulls are. A client
				// that draws a seat glyph rather than a dot needs this to
				// point the chair the right way.
				Rotation: math.Mod(degrees+180, 360),
				Kind:     kind,
				RowOrder: row,
				// Position, not the label, exactly as the linear path. Two
				// seats are neighbours when their orders differ by one.
				SeatOrder: position,
			})
		}
	}
	if len(seats) == 0 {
		return nil, fmt.Errorf("seating: every seat in the block was skipped")
	}
	return seats, nil
}

// TableSpec lays out round tables with seats around them.
//
// The other "rounded" venue, and a different one from an arc: a camarote, a
// gala dinner, a churrascaria floor at a rodeo. Each table is a RING of seats,
// and the tables themselves sit in a grid.
//
// A table is modelled as a ROW: RowLabel is the table's number, SeatOrder runs
// around it. That is not a trick. It means "four seats together" resolves to
// "four seats at the same table", which is exactly what a party booking a table
// means, and it needs no new concept anywhere downstream.
type TableSpec struct {
	// Tables is how many tables the section has.
	Tables int
	// SeatsPerTable is how many seats ring each one.
	SeatsPerTable int
	// TableRadius is the distance from a table's centre to its seats.
	TableRadius float64
	// PerRow is how many tables sit side by side before the grid wraps.
	PerRow int
	// Gap is the distance between the centres of two neighbouring tables.
	Gap float64
	// FirstTable offsets the numbering, for a floor that continues another's.
	FirstTable int
	// OffsetX and OffsetY place the floor of tables in the layout, so a room
	// can hold a stage, a block of chairs and a table floor beside each other.
	OffsetX float64
	OffsetY float64
}

// Generate lays out the section's tables.
func (s TableSpec) Generate(sectionID string) ([]Seat, error) {
	if strings.TrimSpace(sectionID) == "" {
		return nil, ErrInvalidSection
	}
	if s.Tables <= 0 || s.SeatsPerTable <= 0 {
		return nil, fmt.Errorf("seating: a table section needs at least one table with one seat")
	}

	radius := s.TableRadius
	if radius <= 0 {
		radius = 34
	}
	perRow := s.PerRow
	if perRow <= 0 {
		perRow = 4
	}
	gap := s.Gap
	if gap <= 0 {
		gap = radius*2 + 40
	}
	first := s.FirstTable
	if first <= 0 {
		first = 1
	}

	seats := make([]Seat, 0, s.Tables*s.SeatsPerTable)
	for table := 0; table < s.Tables; table++ {
		centreX := s.OffsetX + float64(table%perRow)*gap
		centreY := s.OffsetY + float64(table/perRow)*gap
		tableLabel := strconv.Itoa(first + table)

		for seat := 1; seat <= s.SeatsPerTable; seat++ {
			// Around the whole table, so the last seat does not land on the
			// first. Seat 1 sits at the top and they run clockwise, which is
			// how a place card is laid out.
			degrees := 360 * float64(seat-1) / float64(s.SeatsPerTable)
			radians := degrees * math.Pi / 180

			seats = append(seats, Seat{
				SectionID: sectionID,
				RowLabel:  tableLabel,
				SeatLabel: strconv.Itoa(seat),
				X:         centreX + radius*math.Sin(radians),
				Y:         centreY - radius*math.Cos(radians),
				// Facing the table, because that is what a chair at a table
				// does.
				Rotation:  math.Mod(degrees+180, 360),
				Kind:      SeatStandard,
				RowOrder:  table,
				SeatOrder: seat,
			})
		}
	}
	return seats, nil
}

// label numbers one seat according to the scheme.
func label(numbering Numbering, position, perRow int) string {
	if numbering != NumberingOddEven {
		return strconv.Itoa(position)
	}
	// Outward from the centre. The left half takes the odd numbers rising as it
	// goes out, the right half the evens, which is what the house's own signage
	// says.
	centre := perRow / 2
	if position <= centre {
		return strconv.Itoa((centre-position)*2 + 1)
	}
	return strconv.Itoa((position - centre) * 2)
}

// Compliance is what the law requires of a room, and what it actually has.
//
// Decreto 5.296/2004 art. 23, as amended by Decreto 9.404/2018: a theatre,
// cinema, auditorium, stadium, gym or performance hall must reserve spaces for
// wheelchair users and seats for people with reduced mobility, half of the
// latter built for obese persons.
//
// Computed here, in the domain, because it is arithmetic on a capacity and not
// a database question, and reported rather than enforced, because the platform
// cannot know whether a given room is legally one of those venues. Getting that
// guess wrong in the blocking direction stops a legitimate sale; getting it
// wrong in the warning direction costs a dialog. The organiser carries the
// liability and is the one who has to see the number.
type Compliance struct {
	Capacity int
	// RequiredWheelchair is the wheelchair SPACES the room owes.
	RequiredWheelchair int
	// RequiredReducedMobility is the seats owed to people with reduced
	// mobility and visual disability, including obese persons.
	RequiredReducedMobility int
	// RequiredObese is the half of the above that must be built for obese
	// persons, minimum one.
	RequiredObese int

	HaveWheelchair      int
	HaveReducedMobility int
	HaveObese           int
	HaveCompanion       int
}

// Compliant reports whether every quota is met.
func (c Compliance) Compliant() bool {
	return c.HaveWheelchair >= c.RequiredWheelchair &&
		c.HaveReducedMobility >= c.RequiredReducedMobility &&
		c.HaveObese >= c.RequiredObese &&
		// A wheelchair space is useless without somewhere for a companion to
		// sit, and the law requires the pair.
		c.HaveCompanion >= c.RequiredWheelchair
}

// CheckCompliance measures a laid-out room against the decree.
//
// The scale changes above a thousand places: up to 1,000 it is 2% of capacity
// for each group; beyond that it is 20 spaces plus 1% of the excess, which is
// what stops a 60,000-seat stadium owing 1,200 wheelchair spaces.
func CheckCompliance(seats []Seat, standingCapacity int) Compliance {
	report := Compliance{Capacity: len(seats) + standingCapacity}

	for _, seat := range seats {
		switch seat.Kind {
		case SeatWheelchair:
			report.HaveWheelchair++
		case SeatReducedMobility:
			report.HaveReducedMobility++
		case SeatObese:
			// An obese seat is also a reduced-mobility seat: the decree makes
			// the obese requirement a SUBSET of that quota, not a separate one.
			report.HaveReducedMobility++
			report.HaveObese++
		case SeatCompanion:
			report.HaveCompanion++
		}
	}

	report.RequiredWheelchair = quota(report.Capacity)
	report.RequiredReducedMobility = quota(report.Capacity)
	report.RequiredObese = report.RequiredReducedMobility / 2
	if report.RequiredReducedMobility > 0 && report.RequiredObese < 1 {
		report.RequiredObese = 1
	}
	return report
}

// quota is the decree's own scale: 2% up to a thousand places, then 20 plus 1%
// of the excess. Minimum one, for any room at all.
func quota(capacity int) int {
	if capacity <= 0 {
		return 0
	}
	required := 0
	if capacity <= 1000 {
		// Rounded UP: 2% of 30 places is 0.6, and half a wheelchair space is
		// not a thing a room can provide.
		required = (capacity*2 + 99) / 100
	} else {
		required = 20 + (capacity-1000+99)/100
	}
	if required < 1 {
		required = 1
	}
	return required
}
