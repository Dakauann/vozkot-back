package event

import (
	"context"
	"errors"
	"strings"
	"time"

	domain "vozkot/domain/event"
	mediadomain "vozkot/domain/media"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
)

type EventRepository struct {
	db *gorm.DB
	// trigram records whether pg_trgm is installed. It is read once at
	// construction rather than per query: the answer cannot change while the
	// process runs, and a catalogue search is not the place to ask the
	// catalogue what extensions it has.
	trigram bool
}

var _ domain.Repository = (*EventRepository)(nil)

func NewEventRepository(db *gorm.DB) *EventRepository {
	return &EventRepository{db: db, trigram: hasTrigram(db)}
}

// hasTrigram reports whether typo-tolerant search is available.
//
// Without the extension the `%` similarity operator does not exist and a query
// using it fails outright, so the search degrades to exact word matching rather
// than erroring — slower to find a misspelling, never broken.
func hasTrigram(db *gorm.DB) bool {
	var installed int64
	if err := db.Raw(`SELECT COUNT(*) FROM pg_extension WHERE extname = 'pg_trgm'`).Scan(&installed).Error; err != nil {
		return false
	}
	return installed > 0
}

func (r *EventRepository) Create(ctx context.Context, item *domain.Event) error {
	record := toSchema(item)
	if err := r.db.WithContext(ctx).Create(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return domain.ErrSlugTaken
		}
		return err
	}
	*item = *toDomain(&record)
	return nil
}

func (r *EventRepository) GetByID(ctx context.Context, id string) (*domain.Event, error) {
	return r.one(ctx, "id = ?", id)
}

func (r *EventRepository) GetBySlug(ctx context.Context, slug string) (*domain.Event, error) {
	return r.one(ctx, "slug = ?", strings.TrimSpace(slug))
}

func (r *EventRepository) one(ctx context.Context, query string, args ...any) (*domain.Event, error) {
	var record schema.Event
	if err := r.db.WithContext(ctx).First(&record, append([]any{query}, args...)...).Error; err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

func (r *EventRepository) SlugExists(ctx context.Context, slug string) (bool, error) {
	var found int64
	err := r.db.WithContext(ctx).Model(&schema.Event{}).Where("slug = ?", slug).Count(&found).Error
	return found > 0, err
}

func (r *EventRepository) Update(ctx context.Context, item *domain.Event) error {
	record := toSchema(item)
	result := r.db.WithContext(ctx).Model(&schema.Event{}).Where("id = ?", record.ID).Updates(map[string]any{
		"name":         record.Name,
		"description":  record.Description,
		"category":     record.Category,
		"venue":        record.Venue,
		"address":      record.Address,
		"neighborhood": record.Neighborhood,
		"city":         record.City,
		"uf":           record.UF,
		"postal_code":  record.PostalCode,
		"latitude":     record.Latitude,
		"longitude":    record.Longitude,
		"starts_at":    record.StartsAt,
		"ends_at":      record.EndsAt,
		"status":       record.Status,
		"updated_at":   record.UpdatedAt,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *EventRepository) Delete(ctx context.Context, id string) error {
	result := r.db.WithContext(ctx).Delete(&schema.Event{}, "id = ?", id)
	if result.Error != nil {
		// Tiers reference events with ON DELETE RESTRICT, and those tiers may
		// have orders. Translated here, at the edge where the database's
		// vocabulary ends.
		if errors.Is(result.Error, gorm.ErrForeignKeyViolated) {
			return domain.ErrHasTickets
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// listRow is one event plus the numbers computed alongside it.
type listRow struct {
	schema.Event
	FromPriceCents   *int64
	AvailableTickets int
	// TotalCount is the size of the whole result set, carried on every row by a
	// window function. One query answers both "which page" and "how many
	// pages": running a separate COUNT means two scans of the same predicate,
	// and a count that can disagree with its own rows when something is
	// published between them.
	TotalCount int64
}

// List is the catalogue query.
//
// Everything a card renders is gathered here, in one statement, because a grid
// of twenty-four events asking each for its own cheapest price is twenty-five
// queries — the N+1 that a listing page dies of at exactly the moment it starts
// being popular.
func (r *EventRepository) List(ctx context.Context, filter domain.Filter) (domain.Page, error) {
	filter = filter.Normalize()

	arguments := map[string]any{
		"limit":  filter.Limit,
		"offset": filter.Offset,
	}
	where := []string{"1 = 1"}

	if filter.Status != "" {
		where = append(where, "e.status = @status")
		arguments["status"] = string(filter.Status)
	}
	if filter.OwnerID != "" {
		where = append(where, "e.owner_id = @owner")
		arguments["owner"] = filter.OwnerID
	}
	if len(filter.IDs) > 0 {
		where = append(where, "e.id IN @ids")
		arguments["ids"] = filter.IDs
	}
	if filter.Category != "" {
		where = append(where, "e.category = @category")
		arguments["category"] = string(filter.Category)
	}
	if city := strings.TrimSpace(filter.City); city != "" {
		// unaccent is not assumed, so the fold is done with a lower() on both
		// sides; the city filter is chosen from a list the API supplies, so an
		// exact match after casefolding is what it needs.
		where = append(where, "lower(e.city) = lower(@city)")
		arguments["city"] = city
	}
	if filter.StartsFrom != nil {
		where = append(where, "e.starts_at >= @startsFrom")
		arguments["startsFrom"] = filter.StartsFrom.UTC()
	}
	if filter.StartsUntil != nil {
		where = append(where, "e.starts_at <= @startsUntil")
		arguments["startsUntil"] = filter.StartsUntil.UTC()
	}
	if filter.AvailableOnly {
		where = append(where, "tiers.available_tickets > 0")
	}
	if filter.MaxPriceCents != nil {
		where = append(where, "tiers.from_price_cents IS NOT NULL AND tiers.from_price_cents <= @maxPrice")
		arguments["maxPrice"] = *filter.MaxPriceCents
	}
	if filter.OnlyFree {
		where = append(where, "tiers.from_price_cents = 0")
	}

	ranked := false
	if query := strings.TrimSpace(filter.Query); query != "" {
		arguments["q"] = query
		ranked = true
		if r.trigram {
			// Full text finds the words; trigram finds the typo. A buyer who
			// types "festivl" gets nothing from full-text search alone, and a
			// search box that punishes a misspelling with an empty page is one
			// people stop using.
			where = append(where, `(
				e.search_vector @@ websearch_to_tsquery('portuguese', @q)
				OR e.name % @q
			)`)
		} else {
			where = append(where, `e.search_vector @@ websearch_to_tsquery('portuguese', @q)`)
		}
	}

	order := "e.starts_at ASC, e.id ASC"
	switch filter.Sort {
	case domain.SortRelevance:
		if ranked {
			// ts_rank_cd weighs term proximity as well as frequency, which is
			// what makes "rock in rio" beat a description that mentions rock
			// once and Rio once. The trigram score breaks ties and carries the
			// misspellings, which have no text rank at all.
			rank := "ts_rank_cd(e.search_vector, websearch_to_tsquery('portuguese', @q)) DESC"
			if r.trigram {
				rank += ", similarity(e.name, @q) DESC"
			}
			order = rank + ", e.starts_at ASC, e.id ASC"
		}
	case domain.SortPrice:
		// NULLS LAST: an event with nothing on sale is not "cheapest".
		order = "tiers.from_price_cents ASC NULLS LAST, e.starts_at ASC, e.id ASC"
	case domain.SortNewest:
		order = "e.created_at DESC, e.id ASC"
	}

	statement := `
		SELECT e.*,
		       tiers.from_price_cents,
		       tiers.available_tickets,
		       COUNT(*) OVER() AS total_count
		FROM events e
		LEFT JOIN LATERAL (
			SELECT MIN(t.price_cents) AS from_price_cents,
			       COALESCE(SUM(GREATEST(t.quantity - t.sold - t.reserved, 0)), 0) AS available_tickets
			FROM tickets t
			WHERE t.event_id = e.id AND t.status = 'on_sale'
		) tiers ON TRUE
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY ` + order + `
		LIMIT @limit OFFSET @offset`

	var rows []listRow
	if err := r.db.WithContext(ctx).Raw(statement, arguments).Scan(&rows).Error; err != nil {
		return domain.Page{}, err
	}

	page := domain.Page{Items: make([]domain.Listing, 0, len(rows))}
	for index := range rows {
		page.Items = append(page.Items, domain.Listing{
			Event:            *toDomain(&rows[index].Event),
			FromPriceCents:   rows[index].FromPriceCents,
			AvailableTickets: rows[index].AvailableTickets,
		})
		page.Total = rows[index].TotalCount
	}
	// An empty page still has to report the real total, or a listing on page
	// nine of a filter that now matches three events shows "no results" with no
	// way back.
	if len(rows) == 0 {
		total, err := r.count(ctx, filter)
		if err != nil {
			return domain.Page{}, err
		}
		page.Total = total
	}
	return page, nil
}

// count answers the total when the page itself came back empty, which is the
// only time the window function above has no row to carry it.
func (r *EventRepository) count(ctx context.Context, filter domain.Filter) (int64, error) {
	empty := filter
	empty.Limit = 1
	empty.Offset = 0
	// Re-running the same predicate with no window is cheaper than making every
	// non-empty page pay for a second scan.
	arguments := map[string]any{}
	where := []string{"1 = 1"}
	if empty.Status != "" {
		where = append(where, "e.status = @status")
		arguments["status"] = string(empty.Status)
	}
	if empty.OwnerID != "" {
		where = append(where, "e.owner_id = @owner")
		arguments["owner"] = empty.OwnerID
	}
	if empty.Category != "" {
		where = append(where, "e.category = @category")
		arguments["category"] = string(empty.Category)
	}
	if city := strings.TrimSpace(empty.City); city != "" {
		where = append(where, "lower(e.city) = lower(@city)")
		arguments["city"] = city
	}
	if empty.StartsFrom != nil {
		where = append(where, "e.starts_at >= @startsFrom")
		arguments["startsFrom"] = empty.StartsFrom.UTC()
	}
	if empty.StartsUntil != nil {
		where = append(where, "e.starts_at <= @startsUntil")
		arguments["startsUntil"] = empty.StartsUntil.UTC()
	}
	if query := strings.TrimSpace(empty.Query); query != "" {
		arguments["q"] = query
		if r.trigram {
			where = append(where, `(e.search_vector @@ websearch_to_tsquery('portuguese', @q) OR e.name % @q)`)
		} else {
			where = append(where, `e.search_vector @@ websearch_to_tsquery('portuguese', @q)`)
		}
	}

	var total int64
	err := r.db.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM events e WHERE `+strings.Join(where, " AND "), arguments).Scan(&total).Error
	return total, err
}

// Cities lists the cities with published events, busiest first.
//
// It backs the filter's own options, because a filter that offers a city with
// nothing in it leads every buyer who picks it to an empty page.
func (r *EventRepository) Cities(ctx context.Context, limit int) ([]domain.CityCount, error) {
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	var rows []domain.CityCount
	err := r.db.WithContext(ctx).Raw(`
		SELECT city, MIN(uf) AS uf, COUNT(*) AS count
		FROM events
		WHERE status = ? AND city <> ''
		GROUP BY city
		ORDER BY count DESC, city ASC
		LIMIT ?`, string(domain.StatusPublished), limit).Scan(&rows).Error
	return rows, err
}

func (r *EventRepository) CategoryCounts(ctx context.Context) (map[domain.Category]int64, error) {
	var rows []struct {
		Category string
		Count    int64
	}
	err := r.db.WithContext(ctx).Raw(`
		SELECT category, COUNT(*) AS count
		FROM events
		WHERE status = ?
		GROUP BY category`, string(domain.StatusPublished)).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	counts := make(map[domain.Category]int64, len(rows))
	for _, row := range rows {
		counts[domain.Category(row.Category)] = row.Count
	}
	return counts, nil
}

func translate(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrNotFound
	}
	return err
}

func toSchema(item *domain.Event) schema.Event {
	return schema.Event{
		ID:           item.ID,
		OwnerID:      item.OwnerID,
		Slug:         item.Slug,
		Name:         item.Name,
		Description:  item.Description,
		Category:     string(item.Category),
		Venue:        item.Location.Venue,
		Address:      item.Location.Address,
		Neighborhood: item.Location.Neighborhood,
		City:         item.Location.City,
		UF:           item.Location.UF,
		PostalCode:   item.Location.PostalCode,
		Latitude:     item.Location.Latitude,
		Longitude:    item.Location.Longitude,
		StartsAt:     item.StartsAt,
		EndsAt:       item.EndsAt,
		Status:       string(item.Status),
		CreatedAt:    item.CreatedAt,
		UpdatedAt:    item.UpdatedAt,
	}
}

func toDomain(record *schema.Event) *domain.Event {
	return &domain.Event{
		ID:          record.ID,
		OwnerID:     record.OwnerID,
		Slug:        record.Slug,
		Name:        record.Name,
		Description: record.Description,
		Category:    domain.Category(record.Category),
		Location: domain.Location{
			Venue:        record.Venue,
			Address:      record.Address,
			Neighborhood: record.Neighborhood,
			City:         record.City,
			UF:           record.UF,
			PostalCode:   record.PostalCode,
			Latitude:     record.Latitude,
			Longitude:    record.Longitude,
		},
		StartsAt:  record.StartsAt,
		EndsAt:    record.EndsAt,
		Status:    domain.Status(record.Status),
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
	}
}

// Media is attached by the use case from the media library, so the repository
// never joins it: a listing shows one image and a detail page shows all of
// them, and those are two different reads.
var _ = mediadomain.Media{}

var _ = time.Time{}
