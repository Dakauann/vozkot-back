package ticket

import (
	"context"
	"errors"
	"strings"

	domain "vozkot/domain/ticket"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
)

type TicketRepository struct {
	db *gorm.DB
}

func NewTicketRepository(db *gorm.DB) *TicketRepository {
	return &TicketRepository{db: db}
}

func (r *TicketRepository) Create(ctx context.Context, item *domain.Ticket) error {
	record := toSchema(item)
	if err := r.db.WithContext(ctx).Create(&record).Error; err != nil {
		return err
	}
	*item = *toDomain(&record)
	return nil
}

func (r *TicketRepository) GetByID(ctx context.Context, id string) (*domain.Ticket, error) {
	var record schema.Ticket
	if err := r.db.WithContext(ctx).First(&record, "id = ?", id).Error; err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

func (r *TicketRepository) List(ctx context.Context, filter domain.Filter) ([]domain.Ticket, error) {
	var records []schema.Ticket
	query := applyFilter(r.db.WithContext(ctx).Model(&schema.Ticket{}), filter).Order(orderFor(filter.Sort))
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		query = query.Offset(filter.Offset)
	}
	if err := query.Find(&records).Error; err != nil {
		return nil, err
	}
	items := make([]domain.Ticket, 0, len(records))
	for index := range records {
		items = append(items, *toDomain(&records[index]))
	}
	return items, nil
}

// Count answers the same filter without its window, so a paginated listing can
// report a total the window itself cannot see.
func (r *TicketRepository) Count(ctx context.Context, filter domain.Filter) (int64, error) {
	var total int64
	if err := applyFilter(r.db.WithContext(ctx).Model(&schema.Ticket{}), filter).Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// Update writes only the operator-editable columns. Sold and owner_id are
// deliberately absent: a listing edit must never reassign a ticket or rewrite
// how many were sold.
func (r *TicketRepository) Update(ctx context.Context, item *domain.Ticket) error {
	record := toSchema(item)
	result := r.db.WithContext(ctx).Model(&schema.Ticket{}).Where("id = ?", record.ID).Updates(map[string]any{
		"event_name":  record.EventName,
		"title":       record.Title,
		"description": record.Description,
		"venue":       record.Venue,
		"city":        record.City,
		"starts_at":   record.StartsAt,
		"price_cents": record.PriceCents,
		"currency":    record.Currency,
		"quantity":    record.Quantity,
		"status":      record.Status,
		"updated_at":  record.UpdatedAt,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *TicketRepository) Delete(ctx context.Context, id string) error {
	result := r.db.WithContext(ctx).Delete(&schema.Ticket{}, "id = ?", id)
	if result.Error != nil {
		// The orders table references tickets with ON DELETE RESTRICT, so a
		// tier that has sales cannot be removed. Translated here, at the edge
		// where the database's vocabulary ends.
		if errors.Is(result.Error, gorm.ErrForeignKeyViolated) {
			return domain.ErrHasOrders
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func applyFilter(query *gorm.DB, filter domain.Filter) *gorm.DB {
	if filter.Status != "" {
		query = query.Where("status = ?", string(filter.Status))
	}
	if filter.OwnerID != "" {
		query = query.Where("owner_id = ?", filter.OwnerID)
	}
	if search := strings.TrimSpace(filter.Query); search != "" {
		// Lowercased on both sides so an operator typing "arena" finds "Arena",
		// which is how the venue was actually entered.
		pattern := "%" + strings.ToLower(search) + "%"
		query = query.Where(
			"(LOWER(event_name) LIKE ? OR LOWER(title) LIKE ? OR LOWER(venue) LIKE ? OR LOWER(city) LIKE ?)",
			pattern, pattern, pattern, pattern,
		)
	}
	return query
}

func orderFor(sort domain.Sort) string {
	switch sort {
	case domain.SortCreatedAt:
		return "created_at DESC"
	case domain.SortPrice:
		return "price_cents ASC"
	default:
		// A box office reads its catalogue by door date: the next event first.
		return "starts_at ASC"
	}
}

func translate(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrNotFound
	}
	return err
}

func toSchema(item *domain.Ticket) schema.Ticket {
	return schema.Ticket{
		ID:          item.ID,
		OwnerID:     item.OwnerID,
		EventName:   item.EventName,
		Title:       item.Title,
		Description: item.Description,
		Venue:       item.Venue,
		City:        item.City,
		StartsAt:    item.StartsAt,
		PriceCents:  item.PriceCents,
		Currency:    item.Currency,
		Quantity:    item.Quantity,
		Sold:        item.Sold,
		Reserved:    item.Reserved,
		Status:      string(item.Status),
		CreatedAt:   item.CreatedAt,
		UpdatedAt:   item.UpdatedAt,
	}
}

func toDomain(record *schema.Ticket) *domain.Ticket {
	return &domain.Ticket{
		ID:          record.ID,
		OwnerID:     record.OwnerID,
		EventName:   record.EventName,
		Title:       record.Title,
		Description: record.Description,
		Venue:       record.Venue,
		City:        record.City,
		StartsAt:    record.StartsAt,
		PriceCents:  record.PriceCents,
		Currency:    record.Currency,
		Quantity:    record.Quantity,
		Sold:        record.Sold,
		Reserved:    record.Reserved,
		Status:      domain.Status(record.Status),
		CreatedAt:   record.CreatedAt,
		UpdatedAt:   record.UpdatedAt,
	}
}
