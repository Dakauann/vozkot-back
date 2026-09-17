package main

import (
	"fmt"
	"strings"
	"testing"

	domainseating "vozkot/domain/seating"
)

// roomProblems reports everything wrong with a seeded room, without a database.
//
// The point is to fail in a second rather than halfway through a seed run that
// has already written a hundred and sixty events. It is a function rather than
// a loop inside a test so that a deliberately broken room can be fed to the
// same checks, which is the only way to know they work: the first version of
// this file passed the concert hall and the real run then refused it.
//
// The four ways a seeded room can be wrong:
//
//   - a spec the generator refuses,
//   - two sections drawn on the same floor, which the save refuses,
//   - a seat band named in the price list that no CHAIR is in, which the bind
//     refuses,
//   - chairs drawn with no price, which is stock nobody can buy.
func roomProblems(room seededRoom) []string {
	var problems []string
	note := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	sections := make([]domainseating.Section, 0, len(room.Specs))
	seats := make([]domainseating.Seat, 0, 512)

	for index, spec := range room.Specs {
		id := fmt.Sprintf("sec_%d", index)
		section := domainseating.Section{
			ID:           id,
			Name:         spec.Name,
			Kind:         spec.Kind,
			Capacity:     spec.Capacity,
			OffsetX:      spec.OffsetX,
			OffsetY:      spec.OffsetY,
			Width:        spec.Width,
			Height:       spec.Height,
			Category:     spec.Category,
			DisplayOrder: index + 1,
		}
		if err := section.Validate(); err != nil {
			note("section %q is invalid: %v", spec.Name, err)
			continue
		}
		sections = append(sections, section)

		if !spec.Kind.Seated() {
			continue
		}
		var drawn []domainseating.Seat
		var err error
		if spec.Tables.Tables > 0 {
			drawn, err = spec.Tables.Generate(id)
		} else {
			drawn, err = spec.Rows.Generate(id)
		}
		if err != nil {
			note("section %q does not draw: %v", spec.Name, err)
			continue
		}
		domainseating.RotateBy(drawn, spec.Rotation)
		domainseating.PlaceAt(drawn, spec.OffsetX, spec.OffsetY)
		seats = append(seats, drawn...)
	}

	// Nothing on top of anything, which the save would refuse.
	footprints := make([]domainseating.Footprint, 0, len(sections))
	for _, section := range sections {
		var inside []domainseating.Seat
		for index := range seats {
			if seats[index].SectionID == section.ID {
				inside = append(inside, seats[index])
			}
		}
		footprints = append(footprints, domainseating.FootprintOf(section, inside))
	}
	if err := domainseating.Overlaps(footprints); err != nil {
		note("sections overlap: %v", err)
	}

	// Every seat band has CHAIRS. This assertion has to ask exactly what the
	// bind asks, and the first version did not: it validated through a helper
	// that added a booth's capacity to its band, so a box with no chairs
	// counted as ten seats here and the bind refused it three lines later.
	chairs := seatsPerBand(sections, seats)
	for band := range room.Prices {
		if chairs[band] == 0 {
			note("prices the seat band %q, which has no chairs (bands with chairs: %v)",
				band, chairs)
		}
	}

	// Every counted section has capacity, and is not also a seat band: the two
	// price lists must not name the same thing, or one section sells twice.
	heads := capacityPerBand(sections)
	for band := range room.Counted {
		if heads[band] == 0 {
			note("prices the counted section %q, which has no capacity (sections with capacity: %v)",
				band, heads)
		}
		if _, both := room.Prices[band]; both {
			note("prices %q as both a seat band and a counted section", band)
		}
	}

	// No chair drawn without a price.
	for band, count := range chairs {
		if _, priced := room.Prices[band]; !priced {
			note("draws %d chairs in %q and never prices them", count, band)
		}
	}

	if len(room.Prices) == 0 && len(room.Counted) == 0 {
		note("prices nothing, so it would be drawn and never sold")
	}
	return problems
}

func TestSeededRoomsAreValid(t *testing.T) {
	for _, room := range seededRooms() {
		t.Run(room.Plan, func(t *testing.T) {
			for _, problem := range roomProblems(room) {
				t.Errorf("%s %s", room.Plan, problem)
			}
		})
	}
}

// The checks above are only worth having if they catch the mistake that got
// past them. This is that mistake: a booth has a capacity and no chairs, so
// pricing it as a SEAT band passes anything that counts capacity as seats and
// fails the bind, which walks the layout's chairs.
func TestARoomThatPricesABoothAsASeatBandIsRejected(t *testing.T) {
	room := concertHall()
	price, ok := room.Counted["Camarote esquerdo"]
	if !ok {
		t.Fatalf("the concert hall no longer sells a Camarote esquerdo by the head; update this test")
	}

	// Move it from the counted list to the seat price list, which is exactly
	// the bug the first seed run hit.
	broken := room
	broken.Prices = map[string]int64{"Cadeiras": room.Prices["Cadeiras"], "Camarote esquerdo": price}
	broken.Counted = map[string]int64{"Pista": room.Counted["Pista"]}

	problems := roomProblems(broken)
	found := false
	for _, problem := range problems {
		if strings.Contains(problem, `"Camarote esquerdo"`) && strings.Contains(problem, "no chairs") {
			found = true
		}
	}
	if !found {
		t.Errorf("pricing a booth as a seat band was not caught. problems reported: %v", problems)
	}
}

// The theatre carries every accessibility kind the decree names, because the
// report and the buyer's legend are only visible on a room that has them.
func TestTheSeededTheatreIsAccessible(t *testing.T) {
	var stalls domainseating.RowSpec
	for _, spec := range theatre().Specs {
		if spec.Name == "Plateia" {
			stalls = spec.Rows
		}
	}
	seats, err := stalls.Generate("sec")
	if err != nil {
		t.Fatalf("the stalls do not draw: %v", err)
	}

	present := map[domainseating.SeatKind]int{}
	for index := range seats {
		present[seats[index].Kind]++
	}
	for _, kind := range []domainseating.SeatKind{
		domainseating.SeatWheelchair,
		domainseating.SeatCompanion,
		domainseating.SeatReducedMobility,
		domainseating.SeatObese,
	} {
		if present[kind] == 0 {
			t.Errorf("the seeded stalls have no %s seat", kind)
		}
	}
	// A wheelchair space without a companion beside it is the one pairing the
	// law is explicit about.
	if present[domainseating.SeatCompanion] < present[domainseating.SeatWheelchair] {
		t.Errorf("%d wheelchair spaces but only %d companion seats",
			present[domainseating.SeatWheelchair], present[domainseating.SeatCompanion])
	}
}

// And its front rows cost more than its back, which is the feature price bands
// exist for and the one a fresh database could never show.
func TestTheSeededTheatreHasAPremiumBand(t *testing.T) {
	room := theatre()
	if room.Prices["Plateia Premium"] <= room.Prices["Plateia"] {
		t.Errorf("premium costs %d and the stalls cost %d, which is the wrong way round",
			room.Prices["Plateia Premium"], room.Prices["Plateia"])
	}
}

// The concert hall is the room that shows every section kind at once, which is
// what makes it worth seeding at all.
func TestTheConcertHallSellsBothWays(t *testing.T) {
	room := concertHall()
	kinds := map[domainseating.SectionKind]bool{}
	for _, spec := range room.Specs {
		kinds[spec.Kind] = true
	}
	for _, kind := range []domainseating.SectionKind{
		domainseating.SectionStage,
		domainseating.SectionStanding,
		domainseating.SectionSeated,
		domainseating.SectionBooth,
	} {
		if !kinds[kind] {
			t.Errorf("the concert hall has no %s section", kind)
		}
	}
	if len(room.Prices) == 0 {
		t.Error("nothing is sold by the seat, so the seat map sells nothing")
	}
	if len(room.Counted) == 0 {
		t.Error("nothing is sold by the head, so the standing floor and the boxes are unsellable")
	}
}
