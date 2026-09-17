package main

import (
	"context"
	"fmt"
	"log"

	"gorm.io/gorm"

	authdomain "vozkot/domain/auth"
	domainevent "vozkot/domain/event"
	domainseating "vozkot/domain/seating"
	domainticket "vozkot/domain/ticket"
	eventRepository "vozkot/infra/repositories/event"
	seatingRepository "vozkot/infra/repositories/seating"
	ticketRepository "vozkot/infra/repositories/ticket"
	seatingUsecase "vozkot/usecases/seating"
	ticketUsecase "vozkot/usecases/ticket"
)

// The rooms a seeded installation gets, and the nights that sell from them.
//
// This exists because reserved seating was the one half of the product a fresh
// database had never seen. The catalogue seeded a hundred and sixty events that
// all sold the same way, counted stock by quantity, so the seat map, the price
// bands, the accessibility report and the buyer's chart were only ever visible
// to somebody who built a room by hand first.
//
// It covers every variant a room can hold, because the ones that break are the
// combinations nobody set up: a straight block and a curved one, a floor of
// tables, standing stock beside named chairs, boxes sold whole, scenery of both
// kinds, chairs of every accessibility kind the decree names, a sector whose
// front rows cost more than its back, and two wings sharing one price.

type seatingSummary struct {
	Venues      int
	Layouts     int
	BoundEvents int
	Seats       int
}

// seededRoom is one drawn room and the shape of the night that sells it.
type seededRoom struct {
	Venue string
	Plan  string
	// Category picks which of the seeded events this room is bound to, so a
	// theatre lands on a theatre listing rather than on a rodeo.
	Category domainevent.Category
	Specs    []seatingUsecase.SectionSpec
	// Prices maps a SEAT band to what it costs, in cents. Every band here must
	// have real chairs, because binding assigns each chair of the layout to a
	// tier and then refuses a band no chair turned out to be in. A band left
	// out is drawn but not sold, which is a state worth having in the data.
	Prices map[string]int64
	// Counted prices the sections that sell by the head rather than by the
	// chair: a standing floor, a box sold whole. They are NOT part of the seat
	// price list, because they have no chairs to assign, so they get a plain
	// tier and stay out of the bind.
	//
	// A night can carry both, because seated-ness is a property of an order
	// LINE and not of the event: `Item.Seated()` is `len(SeatIDs) > 0`. So a
	// buyer can take two numbered chairs and a spot on the floor in one order,
	// which is how the venue this models actually sells.
	Counted map[string]int64
}

func seedSeating(ctx context.Context, db *gorm.DB, ownerID string) (seatingSummary, error) {
	var summary seatingSummary

	layouts := seatingRepository.NewLayoutRepository(db)
	seats := seatingRepository.NewSeatRepository(db)
	events := eventRepository.NewEventRepository(db)
	tiers := ticketUsecase.NewService(ticketRepository.NewTicketRepository(db))
	service := seatingUsecase.NewService(layouts, seats, events, newID)
	actor := authdomain.Actor{ID: ownerID, Role: "admin"}

	venues := map[string]string{}
	for _, room := range seededRooms() {
		venueID, known := venues[room.Venue]
		if !known {
			venue, err := service.CreateVenue(ctx, actor,
				seatingUsecase.CreateVenueInput{Name: room.Venue})
			if err != nil {
				return summary, fmt.Errorf("create venue %s: %w", room.Venue, err)
			}
			venueID = venue.ID
			venues[room.Venue] = venueID
			summary.Venues++
		}

		layout, err := service.CreateLayout(ctx, actor, seatingUsecase.CreateLayoutInput{
			VenueID: venueID,
			Name:    room.Plan,
		})
		if err != nil {
			return summary, fmt.Errorf("create plan %s: %w", room.Plan, err)
		}
		sections, drawn, err := service.GenerateLayout(ctx, actor, layout.ID, room.Specs)
		if err != nil {
			return summary, fmt.Errorf("draw %s: %w", room.Plan, err)
		}
		summary.Layouts++
		log.Printf("drew %s at %s: %d sections, %d chairs", room.Plan, room.Venue,
			len(sections), len(drawn))

		// One night per room. The event comes from the catalogue that was just
		// seeded, picked by category so a theatre plan sells a theatre listing.
		event, err := firstEventOfCategory(ctx, db, ownerID, room.Category)
		if err != nil {
			return summary, err
		}
		if event == "" {
			log.Printf("no %s event to bind %s to, leaving the plan drawn and unsold",
				room.Category, room.Plan)
			continue
		}

		// A seated event says so, which is what opens the pricing panel and
		// tells the rest of the interface what kind of night this is.
		if err := db.WithContext(ctx).Exec(
			"UPDATE events SET sales_mode = ? WHERE id = ?",
			string(domainevent.SalesSeated), event).Error; err != nil {
			return summary, fmt.Errorf("mark %s as seated: %w", event, err)
		}

		// A tier per band, sized to the band so nothing is unsellable. The tier
		// is the authority on the money and its quantity caps the sale, so a
		// tier smaller than its band would leave chairs nobody can buy.
		counts := seatsPerBand(sections, drawn)
		heads := capacityPerBand(sections)
		ticketByCategory := map[string]string{}
		ticketBySection := map[string]string{}
		for band, price := range room.Prices {
			quantity := counts[band]
			if quantity == 0 {
				return summary, fmt.Errorf(
					"plan %s prices the seat band %q, which has no chairs", room.Plan, band)
			}
			// The same use case the dashboard calls, so a seeded tier has the
			// same fee, currency and status as one an operator types in.
			tier, err := tiers.Create(ctx, ticketUsecase.CreateInput{
				OwnerID:     ownerID,
				EventID:     event,
				Title:       band,
				Description: "Lugar marcado, com fila e poltrona.",
				PriceCents:  price,
				// Sized to the band. The tier caps the sale, so a tier smaller
				// than its band leaves chairs nobody can buy.
				Quantity: quantity,
				Status:   domainticket.StatusOnSale,
			})
			if err != nil {
				return summary, fmt.Errorf("create the tier %s: %w", band, err)
			}
			ticketByCategory[band] = tier.ID
		}

		// The counted half: a tier each, and no entry in the seat price list.
		// Created after the seat bands so the pricing screen lists the numbered
		// stock first, which is the order a box office reads it in.
		for band, price := range room.Counted {
			quantity := heads[band]
			if quantity == 0 {
				return summary, fmt.Errorf(
					"plan %s prices the counted section %q, which has no capacity",
					room.Plan, band)
			}
			tier, err := tiers.Create(ctx, ticketUsecase.CreateInput{
				OwnerID:     ownerID,
				EventID:     event,
				Title:       band,
				Description: "Entrada por ordem de chegada, sem lugar marcado.",
				PriceCents:  price,
				Quantity:    quantity,
				Status:      domainticket.StatusOnSale,
			})
			if err != nil {
				return summary, fmt.Errorf("create the counted tier %s: %w", band, err)
			}
			for _, section := range sections {
				if !section.Kind.Seated() && !section.Kind.Marker() && domainseating.Category("", section.Category, section.Name) == band {
					ticketBySection[section.ID] = tier.ID
				}
			}
		}

		manifest, err := service.Bind(ctx, actor, seatingUsecase.BindInput{
			EventID:          event,
			LayoutID:         layout.ID,
			TicketByCategory: ticketByCategory,
		})
		if err != nil {
			return summary, fmt.Errorf("put %s on sale: %w", room.Plan, err)
		}
		if err := service.BindAreas(ctx, actor, event, ticketBySection); err != nil {
			return summary, fmt.Errorf("bind counted areas: %w", err)
		}
		summary.BoundEvents++
		summary.Seats += manifest.SeatCount
		log.Printf("put %s on sale for %s: %d chairs across %d bands",
			room.Plan, event, manifest.SeatCount, len(ticketByCategory))
	}

	return summary, nil
}

// seatsPerBand counts the CHAIRS in each price band of a drawn room.
//
// Chairs only, deliberately. An earlier version added a standing or booth
// section's capacity to its band, which reads as helpful and is a lie: binding
// walks the layout's chairs and refuses a priced band that none of them is in,
// so a booth counted here passed the seeder's own guard and then failed the
// bind three lines later. A helper used to validate a price list has to answer
// the same question the bind asks.
func seatsPerBand(sections []domainseating.Section, drawn []domainseating.Seat) map[string]int {
	bySection := make(map[string]domainseating.Section, len(sections))
	for _, section := range sections {
		bySection[section.ID] = section
	}
	counts := map[string]int{}
	for index := range drawn {
		section := bySection[drawn[index].SectionID]
		band := domainseating.Category(drawn[index].Category, section.Category, section.Name)
		counts[band]++
	}
	return counts
}

// capacityPerBand counts the HEADS a room's counted sections hold.
//
// The other half of seatsPerBand: a standing floor or a box sold whole has a
// capacity and no chairs, so this is what sizes its tier.
func capacityPerBand(sections []domainseating.Section) map[string]int {
	counts := map[string]int{}
	for _, section := range sections {
		if section.Kind.Seated() || section.Kind.Marker() {
			continue
		}
		band := domainseating.Category("", section.Category, section.Name)
		counts[band] += section.Capacity
	}
	return counts
}

// firstEventOfCategory finds a seeded event to sell a room from.
func firstEventOfCategory(
	ctx context.Context,
	db *gorm.DB,
	ownerID string,
	category domainevent.Category,
) (string, error) {
	var id string
	err := db.WithContext(ctx).
		Raw(`SELECT id FROM events
		      WHERE owner_id = ? AND category = ?
		        AND id NOT IN (SELECT event_id FROM event_seatings)
		      ORDER BY starts_at
		      LIMIT 1`, ownerID, string(category)).
		Scan(&id).Error
	if err != nil {
		return "", fmt.Errorf("find a %s event: %w", category, err)
	}
	return id, nil
}

// seededRooms is every room a fresh installation gets, and it is deliberately a
// tour of the variants rather than four plausible venues.
func seededRooms() []seededRoom {
	return []seededRoom{
		theatre(),
		concertHall(),
		arena(),
		tableFloor(),
	}
}

// theatre is the commonest paid room, and the one that carries the two features
// a sector cannot express on its own: front rows priced above the rest, and
// every accessibility kind the decree names.
func theatre() seededRoom {
	// Rows A and B of the stalls are premium. Marked per chair, because the
	// front rows of a sector are not a sector.
	premium := map[string]string{}
	for _, row := range []string{"A", "B"} {
		for seat := 1; seat <= 16; seat++ {
			premium[domainseating.SeatKindKey(row, fmt.Sprint(seat))] = "Plateia Premium"
		}
	}
	// The accessible chairs the law requires, in the back row of the stalls
	// where the level entrance is.
	kinds := map[string]domainseating.SeatKind{
		domainseating.SeatKindKey("K", "1"): domainseating.SeatWheelchair,
		domainseating.SeatKindKey("K", "2"): domainseating.SeatCompanion,
		domainseating.SeatKindKey("K", "3"): domainseating.SeatWheelchair,
		domainseating.SeatKindKey("K", "4"): domainseating.SeatCompanion,
		domainseating.SeatKindKey("K", "5"): domainseating.SeatReducedMobility,
		domainseating.SeatKindKey("K", "6"): domainseating.SeatReducedMobility,
		domainseating.SeatKindKey("K", "7"): domainseating.SeatObese,
		domainseating.SeatKindKey("K", "8"): domainseating.SeatObese,
		// A pillar at the end of the balcony, which is a disclosure and not an
		// accessibility kind.
		domainseating.SeatKindKey("L", "1"): domainseating.SeatRestrictedView,
		domainseating.SeatKindKey("L", "2"): domainseating.SeatRestrictedView,
	}

	return seededRoom{
		Venue:    "Teatro Municipal da Vozkot",
		Plan:     "Configuracao padrao",
		Category: domainevent.CategoryTeatrosEspetacs,
		Specs: []seatingUsecase.SectionSpec{
			{
				Name: "Palco", Kind: domainseating.SectionStage,
				OffsetX: 250, OffsetY: 0, Width: 300, Height: 60,
			},
			{
				Name: "Plateia", Kind: domainseating.SectionSeated,
				OffsetX: 220, OffsetY: 140,
				Rows: domainseating.RowSpec{
					Rows: 10, SeatsPerRow: 16,
					// An aisle after the sixth chair, which is what a real house
					// has and what makes adjacency worth testing.
					Skips:           []int{7},
					KindByLabel:     kinds,
					CategoryByLabel: premium,
				},
			},
			{
				Name: "Balcao", Kind: domainseating.SectionSeated,
				OffsetX: 196, OffsetY: 480,
				Rows: domainseating.RowSpec{
					Rows: 4, SeatsPerRow: 18, FirstRowLetter: "L",
					KindByLabel: kinds,
				},
			},
		},
		Prices: map[string]int64{
			"Plateia Premium": 24000,
			"Plateia":         14000,
			"Balcao":          9000,
		},
	}
}

// concertHall mixes all three ways of selling in one room: standing stock by the
// number, named chairs behind it, and individual admissions to either side box.
// Each box has dedicated inventory so the buyer can choose a side.
func concertHall() seededRoom {
	return seededRoom{
		Venue:    "Casa Vozkot",
		Plan:     "Pista e camarotes",
		Category: domainevent.CategoryFestasShows,
		Specs: []seatingUsecase.SectionSpec{
			{
				Name: "Palco", Kind: domainseating.SectionStage,
				OffsetX: 250, OffsetY: 0, Width: 400, Height: 60,
			},
			{
				Name: "Pista", Kind: domainseating.SectionStanding, Capacity: 600,
				OffsetX: 250, OffsetY: 100, Width: 400, Height: 180,
			},
			{
				Name: "Cadeiras", Kind: domainseating.SectionSeated,
				OffsetX: 270, OffsetY: 320,
				Rows: domainseating.RowSpec{Rows: 6, SeatsPerRow: 16},
			},
			{
				Name: "Camarote esquerdo", Kind: domainseating.SectionBooth, Capacity: 10,
				// Each side is independently selectable and has its own capacity.
				Category: "Camarote esquerdo",
				OffsetX:  20, OffsetY: 320, Width: 180, Height: 116,
			},
			{
				Name: "Camarote direito", Kind: domainseating.SectionBooth, Capacity: 10,
				Category: "Camarote direito",
				OffsetX:  700, OffsetY: 320, Width: 180, Height: 116,
			},
		},
		// The showcase room: numbered chairs sold by the seat, a standing floor
		// and two boxes sold by the head, all on one night.
		Prices: map[string]int64{"Cadeiras": 18000},
		Counted: map[string]int64{
			"Pista": 12000,
			// Individual admission to the selected side, never an entire box.
			"Camarote esquerdo": 60000,
			"Camarote direito":  60000,
		},
	}
}

// arena is the round venue: a full ring of numbered stands around a floor, with
// a show stage clear of both. Its rows are numbered rather than lettered,
// because lettering runs out after twenty four and an arena has more.
func arena() seededRoom {
	return seededRoom{
		Venue:    "Arena Vozkot",
		Plan:     "Rodeio",
		Category: domainevent.CategoryEsportivo,
		Specs: []seatingUsecase.SectionSpec{
			{
				Name: "Arquibancada", Kind: domainseating.SectionSeated,
				OffsetX: 0, OffsetY: 0,
				Rows: domainseating.RowSpec{
					Shape: domainseating.ShapeArc, Rows: 4, SeatsPerRow: 24,
					Radius: 180, StartAngle: 0, SweepAngle: 360, SeatPitch: 26,
					RowLabels: domainseating.RowsNumbered,
				},
			},
			// EVERY OFFSET IN A SECTION IS ITS TOP-LEFT, including this
			// block's: the arc generator draws the ring around its own origin
			// and PlaceAt then moves the whole thing so its corner lands on the
			// section's offset. The stands are 4 rows from radius 180 at a row
			// gap of 28, so they span 0..528 and their centre is (264, 264).
			{
				// Concentric with the stands: 114 + 300/2 is that same centre.
				Name: "Arena", Kind: domainseating.SectionArena,
				OffsetX: 114, OffsetY: 114, Width: 300, Height: 300,
			},
			{
				// Clear of the stands, which is the point, but ON THEIR AXIS.
				// At 620,620 it sat diagonally off the corner of the room, far
				// from the ring and facing nothing.
				Name: "Palco", Kind: domainseating.SectionStage,
				OffsetX: 114, OffsetY: 570, Width: 300, Height: 60,
			},
		},
		Prices: map[string]int64{"Arquibancada": 8000},
	}
}

// tableFloor is the gala: a floor of round tables, where a table is a row and
// four seats together means four at the same table.
func tableFloor() seededRoom {
	return seededRoom{
		Venue:    "Salao Vozkot",
		Plan:     "Jantar de gala",
		Category: domainevent.CategoryGastronomia,
		Specs: []seatingUsecase.SectionSpec{
			{
				Name: "Palco", Kind: domainseating.SectionStage,
				OffsetX: 250, OffsetY: 0, Width: 300, Height: 60,
			},
			{
				Name: "Mesas", Kind: domainseating.SectionSeated,
				OffsetX: 204, OffsetY: 140,
				Tables: domainseating.TableSpec{Tables: 12, SeatsPerTable: 8, PerRow: 4},
			},
		},
		Prices: map[string]int64{"Mesas": 32000},
	}
}
