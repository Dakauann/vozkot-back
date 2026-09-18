package seating

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"

	domain "vozkot/domain/seating"
	"vozkot/infra/database/schema"
)

// LayoutRepository is the definition side: venues, layouts, sections, seats.
//
// Separate from SeatRepository because the two carry entirely different traffic.
// A layout is written once by one organiser in an editor; seat status is written
// by every buyer and read on every map poll. Binding them into one type would
// put an editor's concerns inside the transaction that takes money.
type LayoutRepository struct {
	db *gorm.DB
}

func NewLayoutRepository(db *gorm.DB) *LayoutRepository { return &LayoutRepository{db: db} }

var _ domain.LayoutRepository = (*LayoutRepository)(nil)

func (r *LayoutRepository) CreateVenue(ctx context.Context, venue *domain.Venue) error {
	if venue.ID == "" {
		venue.ID = newID("ven")
	}
	record := schema.Venue{
		ID:       venue.ID,
		OwnerID:  venue.OwnerID,
		Name:     venue.Name,
		Capacity: venue.Capacity,
	}
	if err := r.db.WithContext(ctx).Create(&record).Error; err != nil {
		return err
	}
	venue.CreatedAt = record.CreatedAt
	venue.UpdatedAt = record.UpdatedAt
	return nil
}

func (r *LayoutRepository) VenueByID(ctx context.Context, id string) (*domain.Venue, error) {
	var record schema.Venue
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &domain.Venue{
		ID:        record.ID,
		OwnerID:   record.OwnerID,
		Name:      record.Name,
		Capacity:  record.Capacity,
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
	}, nil
}

func (r *LayoutRepository) ListVenues(ctx context.Context, ownerID string, limit, offset int) ([]domain.Venue, int64, error) {
	query := r.db.WithContext(ctx).Model(&schema.Venue{})
	if ownerID != "" {
		query = query.Where("owner_id = ?", ownerID)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 20
	}
	var records []schema.Venue
	if err := query.Order("name").Limit(limit).Offset(offset).Find(&records).Error; err != nil {
		return nil, 0, err
	}
	venues := make([]domain.Venue, 0, len(records))
	for _, record := range records {
		venues = append(venues, domain.Venue{
			ID:        record.ID,
			OwnerID:   record.OwnerID,
			Name:      record.Name,
			Capacity:  record.Capacity,
			CreatedAt: record.CreatedAt,
			UpdatedAt: record.UpdatedAt,
		})
	}
	return venues, total, nil
}

func (r *LayoutRepository) CreateLayout(ctx context.Context, layout *domain.Layout) error {
	if layout.ID == "" {
		layout.ID = newID("lay")
	}
	if layout.Version == 0 {
		layout.Version = 1
	}
	if layout.Status == "" {
		layout.Status = domain.LayoutDraft
	}
	if layout.ViewBoxWidth == 0 {
		layout.ViewBoxWidth = 1000
	}
	if layout.ViewBoxHeight == 0 {
		layout.ViewBoxHeight = 1000
	}
	record := schema.VenueLayout{
		ID:            layout.ID,
		VenueID:       layout.VenueID,
		OwnerID:       layout.OwnerID,
		Name:          layout.Name,
		Version:       layout.Version,
		Status:        string(layout.Status),
		ViewBoxWidth:  layout.ViewBoxWidth,
		ViewBoxHeight: layout.ViewBoxHeight,
	}
	if err := r.db.WithContext(ctx).Create(&record).Error; err != nil {
		return err
	}
	layout.CreatedAt = record.CreatedAt
	layout.UpdatedAt = record.UpdatedAt
	return nil
}

func (r *LayoutRepository) LayoutByID(ctx context.Context, id string) (*domain.Layout, error) {
	var record schema.VenueLayout
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return toDomainLayout(&record), nil
}

func (r *LayoutRepository) ListLayouts(ctx context.Context, venueID string) ([]domain.Layout, error) {
	var records []schema.VenueLayout
	if err := r.db.WithContext(ctx).
		Where("venue_id = ?", venueID).
		Order("name, version DESC").
		Find(&records).Error; err != nil {
		return nil, err
	}
	layouts := make([]domain.Layout, 0, len(records))
	for index := range records {
		layouts = append(layouts, *toDomainLayout(&records[index]))
	}
	if len(layouts) == 0 {
		return layouts, nil
	}

	// One grouped count for the whole list. A detail fetch per plan would be a
	// request per row on the page whose job is comparing them.
	ids := make([]string, 0, len(layouts))
	for index := range layouts {
		ids = append(ids, layouts[index].ID)
	}
	var counted []struct {
		LayoutID string
		Seats    int
	}
	if err := r.db.WithContext(ctx).
		Table("layout_seats AS ls").
		Select("sec.layout_id AS layout_id, count(*) AS seats").
		Joins("JOIN layout_sections AS sec ON sec.id = ls.section_id").
		Where("sec.layout_id IN ?", ids).
		Group("sec.layout_id").
		Scan(&counted).Error; err != nil {
		// A listing without its counts is still a listing. Failing the whole
		// page over a derived number would trade the feature for the detail.
		return layouts, nil
	}
	seats := make(map[string]int, len(counted))
	for _, row := range counted {
		seats[row.LayoutID] = row.Seats
	}
	for index := range layouts {
		layouts[index].SeatCount = seats[layouts[index].ID]
	}
	return layouts, nil
}

func (r *LayoutRepository) PublishLayout(ctx context.Context, id string) error {
	result := r.db.WithContext(ctx).Model(&schema.VenueLayout{}).
		Where("id = ?", id).
		Update("status", string(domain.LayoutPublished))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *LayoutRepository) FreezeLayout(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Model(&schema.VenueLayout{}).
		Where("id = ? AND frozen = false", id).
		Update("frozen", true).Error
}

// ReplaceSections writes a layout's sections and seats in one transaction.
//
// Replace rather than patch, because the editor generates whole sections from a
// row form and a partial write would leave a layout nobody could reason about.
//
// Refused outright on a frozen layout. A frozen layout has an event bound to it
// and possibly seats sold from it, and the only safe edit is a copy at the next
// version, which the use case does, so that the two events keep pointing at
// the rooms they actually sold.
func (r *LayoutRepository) ReplaceSections(
	ctx context.Context,
	layoutID string,
	sections []domain.Section,
	seats []domain.Seat,
) error {
	var layout schema.VenueLayout
	if err := r.db.WithContext(ctx).Where("id = ?", layoutID).First(&layout).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.ErrNotFound
		}
		return err
	}
	if layout.Frozen {
		return fmt.Errorf("seating: layout %s is frozen: %w", layoutID, domain.ErrSeatsSold)
	}

	sectionRows := make([]schema.LayoutSection, 0, len(sections))
	byIndex := make([]string, len(sections))
	for index := range sections {
		section := &sections[index]
		if err := section.Validate(); err != nil {
			return err
		}
		if section.ID == "" {
			section.ID = newID("sec")
		}
		byIndex[index] = section.ID
		shape, err := json.Marshal(section.Shape)
		if err != nil {
			return fmt.Errorf("seating: encode shape of section %s: %w", section.ID, err)
		}
		sectionRows = append(sectionRows, schema.LayoutSection{
			ID:           section.ID,
			LayoutID:     layoutID,
			Name:         section.Name,
			Kind:         string(section.Kind),
			Capacity:     section.Capacity,
			OffsetX:      section.OffsetX,
			OffsetY:      section.OffsetY,
			Width:        section.Width,
			Height:       section.Height,
			Rotation:     section.Rotation,
			Category:     section.Category,
			Shape:        shape,
			Definition:   section.Definition,
			DisplayOrder: section.DisplayOrder,
		})
	}

	seatRows := make([]schema.LayoutSeat, 0, len(seats))
	for index := range seats {
		seat := &seats[index]
		if err := seat.Validate(); err != nil {
			return err
		}
		if seat.ID == "" {
			seat.ID = newID("lst")
		}
		seatRows = append(seatRows, schema.LayoutSeat{
			ID:        seat.ID,
			SectionID: seat.SectionID,
			RowLabel:  seat.RowLabel,
			SeatLabel: seat.SeatLabel,
			X:         seat.X,
			Y:         seat.Y,
			Rotation:  seat.Rotation,
			Kind:      string(seat.Kind),
			Category:  seat.Category,
			RowOrder:  seat.RowOrder,
			SeatOrder: seat.SeatOrder,
		})
	}

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Seats first: they cascade from sections, so deleting the sections
		// would take them anyway, and doing it in this order keeps the
		// statement count the same whether or not the cascade fires.
		if err := tx.Exec(`
			DELETE FROM layout_seats
			 WHERE section_id IN (SELECT id FROM layout_sections WHERE layout_id = ?)`,
			layoutID).Error; err != nil {
			return err
		}
		if err := tx.Where("layout_id = ?", layoutID).
			Delete(&schema.LayoutSection{}).Error; err != nil {
			return err
		}
		if err := upsertAll(tx, sectionRows, []string{
			"name", "kind", "capacity", "shape", "display_order",
		}); err != nil {
			return err
		}
		return upsertAll(tx, seatRows, []string{
			"section_id", "row_label", "seat_label", "x", "y",
			"rotation", "kind", "row_order", "seat_order",
		})
	})
}

func (r *LayoutRepository) SectionsOf(ctx context.Context, layoutID string) ([]domain.Section, error) {
	var records []schema.LayoutSection
	if err := r.db.WithContext(ctx).
		Where("layout_id = ?", layoutID).
		Order("display_order, name").
		Find(&records).Error; err != nil {
		return nil, err
	}
	sections := make([]domain.Section, 0, len(records))
	for _, record := range records {
		section := domain.Section{
			ID:           record.ID,
			LayoutID:     record.LayoutID,
			Name:         record.Name,
			Kind:         domain.SectionKind(record.Kind),
			Capacity:     record.Capacity,
			OffsetX:      record.OffsetX,
			OffsetY:      record.OffsetY,
			Width:        record.Width,
			Height:       record.Height,
			Rotation:     record.Rotation,
			Category:     record.Category,
			Definition:   record.Definition,
			DisplayOrder: record.DisplayOrder,
		}
		if len(record.Shape) > 0 {
			// A shape that will not decode is a painting problem, not a selling
			// one: the section still has its name, its kind and its seats, so
			// the polygon is dropped rather than failing the whole read.
			_ = json.Unmarshal(record.Shape, &section.Shape)
		}
		sections = append(sections, section)
	}
	return sections, nil
}

func (r *LayoutRepository) SeatsOf(ctx context.Context, layoutID string) ([]domain.Seat, error) {
	var records []schema.LayoutSeat
	if err := r.db.WithContext(ctx).Raw(`
		SELECT s.* FROM layout_seats s
		  JOIN layout_sections c ON c.id = s.section_id
		 WHERE c.layout_id = ?
		 ORDER BY c.display_order, s.row_order, s.seat_order`, layoutID,
	).Scan(&records).Error; err != nil {
		return nil, err
	}
	seats := make([]domain.Seat, 0, len(records))
	for _, record := range records {
		seats = append(seats, domain.Seat{
			ID:        record.ID,
			SectionID: record.SectionID,
			RowLabel:  record.RowLabel,
			SeatLabel: record.SeatLabel,
			X:         record.X,
			Y:         record.Y,
			Rotation:  record.Rotation,
			Kind:      domain.SeatKind(record.Kind),
			Category:  record.Category,
			RowOrder:  record.RowOrder,
			SeatOrder: record.SeatOrder,
		})
	}
	return seats, nil
}

func toDomainLayout(record *schema.VenueLayout) *domain.Layout {
	return &domain.Layout{
		ID:            record.ID,
		VenueID:       record.VenueID,
		OwnerID:       record.OwnerID,
		Name:          record.Name,
		Version:       record.Version,
		Status:        domain.LayoutStatus(record.Status),
		Frozen:        record.Frozen,
		ViewBoxWidth:  record.ViewBoxWidth,
		ViewBoxHeight: record.ViewBoxHeight,
		CreatedAt:     record.CreatedAt,
		UpdatedAt:     record.UpdatedAt,
	}
}
