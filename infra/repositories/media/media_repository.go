package media

import (
	"context"
	"errors"

	domain "vozkot/domain/media"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
)

type MediaRepository struct {
	db *gorm.DB
}

func NewMediaRepository(db *gorm.DB) *MediaRepository {
	return &MediaRepository{db: db}
}

func (r *MediaRepository) Create(ctx context.Context, item *domain.Media) error {
	record := toSchema(item)
	if err := r.db.WithContext(ctx).Create(&record).Error; err != nil {
		return err
	}
	*item = *toDomain(&record)
	return nil
}

func (r *MediaRepository) GetByID(ctx context.Context, id string) (*domain.Media, error) {
	var record schema.Media
	if err := r.db.WithContext(ctx).First(&record, "id = ?", id).Error; err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

func (r *MediaRepository) ListByEventID(ctx context.Context, eventID string) ([]domain.Media, error) {
	var records []schema.Media
	if err := r.db.WithContext(ctx).
		Where("event_id = ?", eventID).
		Order("position ASC, created_at ASC").
		Find(&records).Error; err != nil {
		return nil, err
	}
	return toDomainSlice(records), nil
}

// ListByEventIDs loads many galleries in one query, which is what keeps a
// listing of twenty tickets from issuing twenty-one round trips.
func (r *MediaRepository) ListByEventIDs(ctx context.Context, eventIDs []string) (map[string][]domain.Media, error) {
	galleries := make(map[string][]domain.Media, len(eventIDs))
	if len(eventIDs) == 0 {
		return galleries, nil
	}
	var records []schema.Media
	if err := r.db.WithContext(ctx).
		Where("event_id IN ?", eventIDs).
		Order("position ASC, created_at ASC").
		Find(&records).Error; err != nil {
		return nil, err
	}
	for index := range records {
		item := toDomain(&records[index])
		galleries[item.EventID] = append(galleries[item.EventID], *item)
	}
	return galleries, nil
}

func (r *MediaRepository) CountByEventID(ctx context.Context, eventID string) (int, error) {
	var total int64
	if err := r.db.WithContext(ctx).Model(&schema.Media{}).Where("event_id = ?", eventID).Count(&total).Error; err != nil {
		return 0, err
	}
	return int(total), nil
}

// NextPosition appends to the end of the gallery. Removing an item from the
// middle leaves a gap in the sequence, which is harmless: only the order the
// numbers imply is ever read, never their spacing.
func (r *MediaRepository) NextPosition(ctx context.Context, eventID string) (int, error) {
	var highest *int
	if err := r.db.WithContext(ctx).Model(&schema.Media{}).
		Where("event_id = ?", eventID).
		Select("MAX(position)").
		Scan(&highest).Error; err != nil {
		return 0, err
	}
	if highest == nil {
		return 0, nil
	}
	return *highest + 1, nil
}

func (r *MediaRepository) Delete(ctx context.Context, id string) error {
	result := r.db.WithContext(ctx).Delete(&schema.Media{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// DeleteByEventID returns what it removed so the caller can drop the matching
// objects from the bucket; deleting the rows alone would orphan the bytes.
func (r *MediaRepository) DeleteByEventID(ctx context.Context, eventID string) ([]domain.Media, error) {
	items, err := r.ListByEventID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return items, nil
	}
	if err := r.db.WithContext(ctx).Delete(&schema.Media{}, "event_id = ?", eventID).Error; err != nil {
		return nil, err
	}
	return items, nil
}

func translate(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrNotFound
	}
	return err
}

func toSchema(item *domain.Media) schema.Media {
	return schema.Media{
		ID:            item.ID,
		EventID:       item.EventID,
		Kind:          string(item.Kind),
		StorageKey:    item.StorageKey,
		URL:           item.URL,
		ContentType:   item.ContentType,
		SizeBytes:     item.SizeBytes,
		Position:      item.Position,
		Width:         item.Width,
		Height:        item.Height,
		BlurDataURL:   item.BlurDataURL,
		DominantColor: item.DominantColor,
		CreatedAt:     item.CreatedAt,
	}
}

func toDomain(record *schema.Media) *domain.Media {
	return &domain.Media{
		ID:            record.ID,
		EventID:       record.EventID,
		Kind:          domain.Kind(record.Kind),
		StorageKey:    record.StorageKey,
		URL:           record.URL,
		ContentType:   record.ContentType,
		SizeBytes:     record.SizeBytes,
		Position:      record.Position,
		Width:         record.Width,
		Height:        record.Height,
		BlurDataURL:   record.BlurDataURL,
		DominantColor: record.DominantColor,
		CreatedAt:     record.CreatedAt,
	}
}

func toDomainSlice(records []schema.Media) []domain.Media {
	items := make([]domain.Media, 0, len(records))
	for index := range records {
		items = append(items, *toDomain(&records[index]))
	}
	return items
}
