package seating

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	domain "vozkot/domain/seating"
	"vozkot/infra/database/schema"
)

// materialiseBatch is how many seat rows go in one INSERT.
//
// A 2,000-seat house is one statement at this size and a 60,000-seat stadium is
// thirty. Unbatched, GORM builds a single statement with 60,000 value tuples
// and PostgreSQL's parameter limit (65,535) turns a legitimate stadium into a
// failed publish.
const materialiseBatch = 500

// Materialise lays out an event's sellable seats from a published layout.
//
// This is the only write in the feature that touches thousands of rows, and it
// happens once per event, before anything is on sale. It is therefore allowed
// to be slow and must be exactly right; everything on the hot path afterwards
// is one indexed update.
//
// Idempotent by REFUSAL rather than by merge. An event that already has a map
// gets ErrAlreadyMaterialised, because the alternatives are both worse: a merge
// silently duplicates chairs when the layout changed, and a delete-and-recreate
// would drop seats somebody is holding. The unique index on
// (event_id, layout_seat_id) is the backstop if this check is ever raced.
func (r *SeatRepository) Materialise(ctx context.Context, plan domain.MaterialisePlan) (domain.EventSeating, error) {
	if plan.EventID == "" {
		return domain.EventSeating{}, domain.ErrInvalidEvent
	}
	if len(plan.TicketBySection) == 0 {
		return domain.EventSeating{}, domain.ErrInvalidTicket
	}

	existing, err := r.SeatingOf(ctx, plan.EventID)
	if err != nil {
		return domain.EventSeating{}, err
	}
	if existing != nil {
		return domain.EventSeating{}, domain.ErrAlreadyMaterialised
	}

	var layout schema.VenueLayout
	if err := r.db.WithContext(ctx).Where("id = ?", plan.LayoutID).First(&layout).Error; err != nil {
		return domain.EventSeating{}, err
	}
	if !domain.LayoutStatus(layout.Status).Sellable() {
		return domain.EventSeating{}, fmt.Errorf(
			"seating: layout %s is %s and cannot be sold from", layout.ID, layout.Status)
	}

	// Only the sections the plan priced. A section left out of the map is not
	// sold at all, which is how an organiser closes the balcony for a night
	// without editing the room.
	sectionIDs := make([]string, 0, len(plan.TicketBySection))
	for sectionID := range plan.TicketBySection {
		sectionIDs = append(sectionIDs, sectionID)
	}

	var sections []schema.LayoutSection
	if err := r.db.WithContext(ctx).
		Where("layout_id = ? AND id IN ?", plan.LayoutID, sectionIDs).
		Find(&sections).Error; err != nil {
		return domain.EventSeating{}, err
	}
	if len(sections) != len(sectionIDs) {
		return domain.EventSeating{}, fmt.Errorf(
			"seating: plan names %d section(s) of layout %s, %d exist",
			len(sectionIDs), plan.LayoutID, len(sections))
	}

	nameOf := make(map[string]string, len(sections))
	seatedSections := make([]string, 0, len(sections))
	for _, section := range sections {
		nameOf[section.ID] = section.Name
		// Standing and booth sections lay out no seats: Pista is a counter and
		// a camarote is one unit admitting many. Both keep selling exactly as
		// they do today, through the tier, which is what makes a mixed house
		// one event rather than three.
		if domain.SectionKind(section.Kind).Seated() {
			seatedSections = append(seatedSections, section.ID)
		}
	}

	var layoutSeats []schema.LayoutSeat
	if len(seatedSections) > 0 {
		if err := r.db.WithContext(ctx).
			Where("section_id IN ?", seatedSections).
			Order("section_id, row_order, seat_order").
			Find(&layoutSeats).Error; err != nil {
			return domain.EventSeating{}, err
		}
	}

	now := time.Now().UTC()
	rows := make([]schema.EventSeat, 0, len(layoutSeats))
	for index := range layoutSeats {
		seat := &layoutSeats[index]
		layoutSeatID := seat.ID
		rows = append(rows, schema.EventSeat{
			ID:       newID("ste"),
			EventID:  plan.EventID,
			TicketID: plan.TicketBySection[seat.SectionID],
			// Provenance, so an editor can trace a sold chair back to the room.
			LayoutSeatID: &layoutSeatID,
			// The snapshot. Everything a ticket, a door panel or an email ever
			// says about this seat is copied here and never read back from the
			// layout, so re-lettering the room next season cannot rewrite it.
			SectionName: nameOf[seat.SectionID],
			RowLabel:    seat.RowLabel,
			SeatLabel:   seat.SeatLabel,
			Kind:        seat.Kind,
			Status:      string(domain.StatusAvailable),
			// The geometry travels with the labels. Without it the buyer's map
			// has no shape at all and an arc draws as a straight row.
			X:         seat.X,
			Y:         seat.Y,
			RowOrder:  seat.RowOrder,
			SeatOrder: seat.SeatOrder,
			UpdatedAt: now,
		})
	}

	manifest := schema.EventSeating{
		EventID:        plan.EventID,
		LayoutID:       layout.ID,
		LayoutVersion:  layout.Version,
		SeatCount:      len(rows),
		MaterialisedAt: now,
	}

	// One transaction for the manifest, the seats and the freeze. A map that
	// half-exists is an event selling some of its chairs, and a layout left
	// unfrozen after seats were cut from it is a room somebody can still edit
	// under a night that is already selling.
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&manifest).Error; err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := tx.CreateInBatches(&rows, materialiseBatch).Error; err != nil {
				return err
			}
			// Stamp every seat with a real version, in one statement after the
			// insert rather than per row during it.
			//
			// Without this they are all born at version 0, and a client that
			// fetched the whole map would compute a cursor of 0 — which
			// ListByEvent reads as "send me everything" — so its first delta
			// poll would re-download the entire house. It matters most on the
			// biggest maps, where it is also least affordable.
			//
			// One statement because CreateInBatches cannot call nextval() per
			// row, and a sequence per row is not needed anyway: what the cursor
			// requires is that these are all below whatever a later change
			// gets, which a single pass guarantees.
			if err := tx.Exec(`
				UPDATE event_seats SET version = `+nextVersion+`
				 WHERE event_id = ?`, plan.EventID).Error; err != nil {
				return err
			}
		}
		// Frozen from the moment an event binds to it, not from the first sale.
		// Waiting for the sale leaves a window where an organiser edits the row
		// letters of a layout an on-sale event is already quoting.
		return tx.Model(&schema.VenueLayout{}).
			Where("id = ? AND frozen = false", layout.ID).
			Update("frozen", true).Error
	})
	if err != nil {
		return domain.EventSeating{}, err
	}

	return domain.EventSeating{
		EventID:        manifest.EventID,
		LayoutID:       manifest.LayoutID,
		LayoutVersion:  manifest.LayoutVersion,
		SeatCount:      manifest.SeatCount,
		MaterialisedAt: manifest.MaterialisedAt,
	}, nil
}

// Dematerialise takes a seat map away from an event that has not sold from it.
//
// Refused the moment one seat is held or sold: the tickets people hold name
// those chairs. Not on the Repository port, because it is an editor action and
// nothing on the money path may reach it.
func (r *SeatRepository) Dematerialise(ctx context.Context, eventID string) error {
	var spoken int64
	if err := r.db.WithContext(ctx).Model(&schema.EventSeat{}).
		Where("event_id = ? AND status IN ?", eventID,
			[]string{string(domain.StatusHeld), string(domain.StatusSold)}).
		Count(&spoken).Error; err != nil {
		return err
	}
	if spoken > 0 {
		return domain.ErrSeatsSold
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("event_id = ?", eventID).Delete(&schema.EventSeat{}).Error; err != nil {
			return err
		}
		return tx.Where("event_id = ?", eventID).Delete(&schema.EventSeating{}).Error
	})
}

// Upsert is how the layout editor writes sections and seats.
//
// ON CONFLICT rather than delete-and-insert, so regenerating a section that an
// event already materialised from keeps the ids its event seats point at.
func upsertAll[T any](tx *gorm.DB, rows []T, columns []string) error {
	if len(rows) == 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns(columns),
	}).CreateInBatches(&rows, materialiseBatch).Error
}

// newID mints a prefixed identifier in the shape the rest of the codebase uses.
func newID(prefix string) string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return prefix + "_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return prefix + "_" + hex.EncodeToString(buffer)
}
