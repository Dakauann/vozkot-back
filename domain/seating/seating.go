// Package seating is reserved seating: the named chair a buyer holds, as
// opposed to the counted stock a party sells.
//
// The distinction is the whole package. A tier ("Pista", "Camarote") is stock
// measured as a number, and one conditional update over one row keeps it from
// overselling. A seat is stock with an identity, and "how many are left" is no
// longer the question anybody is asking: two buyers both want FILA K, POLTRONA
// 12, and exactly one of them may have it.
//
// What this package deliberately does NOT own:
//
//   - Price. A seat points at a tier, and the tier owns the money, the
//     currency, the fee and the sale status. Reserved seating adds no second
//     pricing concept, which is what keeps the fee model, the reports and the
//     refund rules working unchanged.
//   - The order. A seat knows the id of the order holding it, as a string, and
//     nothing else about it. domain/order does not import this package and this
//     package does not import domain/order.
//   - Geometry rendering. Coordinates are stored and handed out; what a map
//     looks like is the client's problem.
package seating

import (
	"errors"
	"sort"
	"strings"
)

var (
	ErrNotFound = errors.New("seat not found")
	// ErrSeatsUnavailable means at least one requested seat was not available
	// when the claim ran. The claim is all-or-nothing, so none of them were
	// taken. Which ones failed is carried on ClaimResult, because "one of your
	// four seats went" is not something a buyer can act on.
	ErrSeatsUnavailable = errors.New("one or more seats are no longer available")
	// ErrNoSeats refuses a claim for nothing. A seated line with no seats is a
	// caller bug, not an empty basket.
	ErrNoSeats           = errors.New("a seated line must name at least one seat")
	ErrInvalidEvent      = errors.New("a seat must belong to an event")
	ErrInvalidTicket     = errors.New("a seat must belong to a ticket tier")
	ErrInvalidLabel      = errors.New("a seat needs a row and a seat label")
	ErrInvalidSeatKind   = errors.New("seat kind is invalid")
	ErrInvalidSection    = errors.New("a seat must belong to a section")
	ErrInvalidSectionKnd = errors.New("section kind is invalid")
	// ErrAlreadyMaterialised refuses to lay out an event's seats twice. The
	// second run would either duplicate every seat or silently diverge from
	// the first, and both are worse than making the caller say which it meant.
	ErrAlreadyMaterialised = errors.New("this event already has a seat map")
	// ErrSeatsSold refuses to take a seat map away from an event that has sold
	// one. The tickets people hold name those seats.
	ErrSeatsSold = errors.New("this event has sold seats and its map cannot be replaced")
)

// Status is where one seat of one event stands.
//
// Four states, and the transitions between them are the same shape as the tier
// counters they project onto: available -> held -> sold is reserve, commit; and
// held -> available is release.
type Status string

const (
	// StatusAvailable is on sale and unclaimed.
	StatusAvailable Status = "available"
	// StatusHeld is claimed by an order that has not paid. It is neither sold
	// nor available, and it returns to available when the hold lapses.
	StatusHeld Status = "held"
	// StatusSold was paid for. Terminal, except for a re-seat.
	StatusSold Status = "sold"
	// StatusBlocked was withheld from sale by the organiser: house seats, a
	// broken chair, a sightline kill, a distancing gap. Never claimable.
	StatusBlocked Status = "blocked"
)

// Claimable reports whether a claim may take this seat.
func (s Status) Claimable() bool { return s == StatusAvailable }

// Occupied reports whether this seat is spoken for, held or sold.
//
// It is what "how many of this tier are gone" counts, and it is deliberately
// NOT the same question as Claimable: a blocked seat is unclaimable and
// unoccupied, because nobody has it.
func (s Status) Occupied() bool { return s == StatusHeld || s == StatusSold }

func (s Status) valid() bool {
	switch s {
	case StatusAvailable, StatusHeld, StatusSold, StatusBlocked:
		return true
	}
	return false
}

// SeatKind is what kind of chair this physically is.
//
// It carries the law. Decreto 5.296/2004 art. 23, as amended by Decreto
// 9.404/2018, requires a Brazilian house to reserve spaces for wheelchair users
// and seats for people with reduced mobility, half of those built for obese
// persons — which is exactly the legend on any Brazilian seat map: "cadeira
// para obesos", "mobilidade reduzida".
//
// Marking them is not decoration. An accessible seat sold to a buyer who did
// not need one is a wheelchair user arriving to a chair, and the organiser
// carries that liability. The kind is what lets the picker hold them back and
// what lets the editor count them.
//
// "Broken" is NOT a kind. A broken chair is an ordinary chair that is blocked,
// and the reason belongs on the block; a kind describes what the chair IS, and
// it survives the repair.
type SeatKind string

const (
	SeatStandard SeatKind = "standard"
	// SeatWheelchair is a SPACE, not a chair: there is no seat to sit on.
	SeatWheelchair SeatKind = "wheelchair"
	// SeatCompanion sits beside a wheelchair space. Its own kind, because the
	// law requires the pair and the picker has to offer them together.
	SeatCompanion SeatKind = "companion"
	// SeatReducedMobility is reachable without stairs and has room to transfer.
	SeatReducedMobility SeatKind = "reduced_mobility"
	// SeatObese is dimensioned for obese persons: wider, reinforced.
	SeatObese SeatKind = "obese"
	// SeatRestrictedView sees part of the stage. Not an accessibility kind; a
	// disclosure, and usually a lower price.
	SeatRestrictedView SeatKind = "restricted_view"
)

// Accessible reports whether this kind exists to satisfy an accessibility
// requirement, and therefore must not be handed to a buyer who did not ask.
func (k SeatKind) Accessible() bool {
	switch k {
	case SeatWheelchair, SeatCompanion, SeatReducedMobility, SeatObese:
		return true
	}
	return false
}

func (k SeatKind) valid() bool {
	switch k {
	case SeatStandard, SeatWheelchair, SeatCompanion, SeatReducedMobility,
		SeatObese, SeatRestrictedView:
		return true
	}
	return false
}

// SectionKind is how a whole block of the room is sold.
//
// Three kinds, because a Brazilian house is usually all three at once and a
// model that cannot say so cannot sell it:
//
//   - Seated: named chairs. This package's reason to exist.
//   - Standing: a counter. Pista. No seats are laid out at all; the tier's own
//     quantity is the authority, exactly as it is today.
//   - Booth: one sellable unit admitting several people. A camarote for ten is
//     not ten chairs, and selling it as ten breaks both the price and the door.
type SectionKind string

const (
	SectionSeated   SectionKind = "seated"
	SectionStanding SectionKind = "standing"
	SectionBooth    SectionKind = "booth"
	// SectionStage is the stage, and SectionArena the floor a rodeo or a
	// circus performs on. Neither holds a seat; both are things in the room
	// that a buyer orients themselves by.
	//
	// They are sections rather than a property of the layout because that is
	// what they are: objects with a position and a size. As a layout-level
	// enum a stage could only ever be in one place, could not be moved, and a
	// room could not have both an arena and a show stage — which a rodeo does.
	SectionStage SectionKind = "stage"
	SectionArena SectionKind = "arena"
)

// Marker reports whether this section is scenery rather than inventory: a
// thing the map draws and nothing sells.
func (k SectionKind) Marker() bool { return k == SectionStage || k == SectionArena }

// Seated reports whether this section lays out individual named seats.
func (k SectionKind) Seated() bool { return k == SectionSeated }

func (k SectionKind) valid() bool {
	switch k {
	case SectionSeated, SectionStanding, SectionBooth, SectionStage, SectionArena:
		return true
	}
	return false
}

// BlockReason says why a seat is not for sale. Free text from a closed set,
// stored so an organiser can find their own house seats again.
type BlockReason string

const (
	BlockHouse       BlockReason = "house"
	BlockProduction  BlockReason = "production"
	BlockBroken      BlockReason = "broken"
	BlockDistancing  BlockReason = "distancing"
	BlockUnspecified BlockReason = ""
)

// Category resolves the price band a seat belongs to.
//
// Three levels, most specific first: the seat's own category, then its
// section's, then the section's NAME. That last fallback is what makes an
// ordinary room need no pricing decisions at all — a plateia is one band called
// "Plateia" because that is what it is called, and an organiser who never opens
// the pricing controls gets exactly the behaviour they had before categories
// existed.
//
// A name and not an id, deliberately. Two sections can share a band — "Plateia
// Esquerda" and "Plateia Direita" both priced as "Plateia" is one dropdown
// instead of two — and that is only expressible if the key is the band's name.
//
// One function because three callers need the same answer: the materialise that
// assigns a tier to a chair, the validation that refuses a price list naming a
// band the room does not have, and the API that hands the resolved band to the
// editor so the editor never has to repeat this.
func Category(seatCategory, sectionCategory, sectionName string) string {
	if band := strings.TrimSpace(seatCategory); band != "" {
		return band
	}
	if band := strings.TrimSpace(sectionCategory); band != "" {
		return band
	}
	return strings.TrimSpace(sectionName)
}

// BandsOf lists a room's price bands, in the order their colour is assigned.
//
// Every band gets a hue, and the ORDER is the hue: slot one is blue, slot two is
// orange, and so on down a fixed categorical palette. That makes the order
// load-bearing rather than cosmetic, so it is computed once, here, and handed to
// every surface that draws the room — the organiser's canvas, the buyer's map,
// the preview and both legends. Two answers to "what colour is Plateia" is a
// room that changes colour when you walk between screens.
//
// Stable by construction: first appearance walking the sections as they are
// DISPLAYED, then their chairs in the order an usher reads them. A band added
// later appends rather than inserting, so naming a new one does not repaint the
// room somebody has just learned to read.
func BandsOf(sections []Section, seats []Seat) []string {
	order := make(map[string]int, len(sections))
	for index, section := range sections {
		order[section.ID] = index
	}
	// The section's own band comes first for each section, because a section
	// declares its band before any of its chairs override it.
	sorted := make([]Section, len(sections))
	copy(sorted, sections)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].DisplayOrder < sorted[j].DisplayOrder
	})

	seen := map[string]bool{}
	bands := make([]string, 0, 4)
	take := func(band string) {
		if band == "" || seen[band] {
			return
		}
		seen[band] = true
		bands = append(bands, band)
	}

	bySection := make(map[string][]Seat, len(sections))
	for index := range seats {
		bySection[seats[index].SectionID] = append(bySection[seats[index].SectionID], seats[index])
	}

	for _, section := range sorted {
		if section.Kind.Marker() {
			continue
		}
		take(Category("", section.Category, section.Name))
		inside := bySection[section.ID]
		sort.SliceStable(inside, func(i, j int) bool {
			if inside[i].RowOrder != inside[j].RowOrder {
				return inside[i].RowOrder < inside[j].RowOrder
			}
			return inside[i].SeatOrder < inside[j].SeatOrder
		})
		for index := range inside {
			take(Category(inside[index].Category, section.Category, section.Name))
		}
	}
	return bands
}

// Label is a seat's name as a human reads it, and as it must keep reading on a
// ticket after the venue renumbers.
//
// Three parts, in the order a Brazilian usher says them: "Plateia A, fila K,
// poltrona 12".
type Label struct {
	Section string
	Row     string
	Seat    string
}

// String renders the label for a ticket, a door panel or an email.
//
// Built here rather than at each surface: the door, the wallet, the receipt and
// the confirmation email all have to say the same thing, and four copies of a
// format string is four chances to disagree about a seat somebody is standing
// in front of.
func (l Label) String() string {
	parts := make([]string, 0, 3)
	if section := strings.TrimSpace(l.Section); section != "" {
		parts = append(parts, section)
	}
	if row := strings.TrimSpace(l.Row); row != "" {
		parts = append(parts, "Fila "+row)
	}
	if seat := strings.TrimSpace(l.Seat); seat != "" {
		parts = append(parts, "Assento "+seat)
	}
	return strings.Join(parts, " · ")
}

// Empty reports whether this label names nothing, which is what a counted line
// carries.
func (l Label) Empty() bool {
	return strings.TrimSpace(l.Section) == "" &&
		strings.TrimSpace(l.Row) == "" &&
		strings.TrimSpace(l.Seat) == ""
}
