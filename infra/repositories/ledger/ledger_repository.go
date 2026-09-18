// Package ledger persists the organiser balance ledger.
package ledger

import (
	"context"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	domain "vozkot/domain/ledger"
	"vozkot/infra/database/schema"
)

type LedgerRepository struct {
	db *gorm.DB
}

var _ domain.Repository = (*LedgerRepository)(nil)

func NewLedgerRepository(db *gorm.DB) *LedgerRepository { return &LedgerRepository{db: db} }

// Append writes entries and ignores any id already present.
//
// ON CONFLICT DO NOTHING rather than a prior SELECT, because the race this
// closes is a settle path running twice at once: two webhook deliveries, or a
// worker reclaimed mid-flight, and a read-then-write loses that race by
// writing two accruals for one order. The ids come from the accrual, which
// derives them from the order, so the second run computes the same ids and the
// database discards them.
func (r *LedgerRepository) Append(ctx context.Context, entries []domain.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	rows := make([]schema.LedgerEntry, 0, len(entries))
	for _, entry := range entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		rows = append(rows, schema.LedgerEntry{
			ID:          entry.ID,
			OrganiserID: entry.OrganiserID,
			EventID:     entry.EventID,
			OrderID:     entry.OrderID,
			Kind:        string(entry.Kind),
			AmountCents: entry.AmountCents,
			AvailableAt: entry.AvailableAt.UTC(),
			CreatedAt:   entry.CreatedAt.UTC(),
			Note:        entry.Note,
		})
	}
	return r.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).
		Create(&rows).Error
}

// BalanceOf is domain.BalanceOf, expressed as one aggregate.
//
// The four numbers are computed by the database rather than by reading rows
// into Go, because an organiser with a season of sales has hundreds of
// thousands of them and the answer is four integers. The CASE arms are the same
// three branches the pure function walks, in the same order, and the package's
// test runs both over identical rows to prove they have not drifted.
func (r *LedgerRepository) BalanceOf(ctx context.Context, organiserID string, now time.Time) (domain.Balance, error) {
	var row struct {
		Available int64
		Pending   int64
		Reserved  int64
		Total     int64
	}
	err := r.db.WithContext(ctx).Model(&schema.LedgerEntry{}).
		Select(`
			COALESCE(SUM(CASE WHEN available_at <= @now THEN amount_cents END), 0) AS available,
			COALESCE(SUM(CASE WHEN available_at >  @now AND kind <> 'reserve' THEN amount_cents END), 0) AS pending,
			COALESCE(SUM(CASE WHEN available_at >  @now AND kind =  'reserve' THEN amount_cents END), 0) AS reserved,
			COALESCE(SUM(amount_cents), 0) AS total`,
			map[string]any{"now": now.UTC()}).
		Where("organiser_id = ?", organiserID).
		Scan(&row).Error
	if err != nil {
		return domain.Balance{}, err
	}
	return domain.Balance{
		AvailableCents: row.Available,
		PendingCents:   row.Pending,
		ReservedCents:  row.Reserved,
		TotalCents:     row.Total,
	}, nil
}

// AccruedOn is what one order has put on the ledger, and what has been taken
// back off it.
//
// Split by sign rather than by kind, so a chargeback, a refund and a manual
// adjustment against the order all count as reversal without this query having
// to be extended every time a new kind is added.
func (r *LedgerRepository) AccruedOn(ctx context.Context, orderID string) (domain.Accrued, error) {
	var row struct {
		Earned   int64
		Reversed int64
	}
	err := r.db.WithContext(ctx).Model(&schema.LedgerEntry{}).
		Select(`
			COALESCE(SUM(CASE WHEN amount_cents > 0 THEN amount_cents END), 0)  AS earned,
			COALESCE(-SUM(CASE WHEN amount_cents < 0 THEN amount_cents END), 0) AS reversed`).
		Where("order_id = ?", orderID).
		Scan(&row).Error
	if err != nil {
		return domain.Accrued{}, err
	}
	return domain.Accrued{EarnedCents: row.Earned, ReversedCents: row.Reversed}, nil
}

func (r *LedgerRepository) List(ctx context.Context, filter domain.Filter) (domain.Page, error) {
	query := r.db.WithContext(ctx).Model(&schema.LedgerEntry{})
	if filter.OrganiserID != "" {
		query = query.Where("organiser_id = ?", filter.OrganiserID)
	}
	if filter.EventID != "" {
		query = query.Where("event_id = ?", filter.EventID)
	}
	if filter.OrderID != "" {
		query = query.Where("order_id = ?", filter.OrderID)
	}
	if filter.Kind != "" {
		query = query.Where("kind = ?", string(filter.Kind))
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return domain.Page{}, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []schema.LedgerEntry
	// Newest first, then by id: created_at alone is not a total order at the
	// volume this table reaches, and a page boundary that falls inside a tied
	// timestamp would show one row twice and skip another.
	if err := query.Order("created_at DESC, id DESC").
		Limit(limit).Offset(max(filter.Offset, 0)).Find(&rows).Error; err != nil {
		return domain.Page{}, err
	}
	items := make([]domain.Entry, 0, len(rows))
	for _, row := range rows {
		items = append(items, domain.Entry{
			ID:          row.ID,
			OrganiserID: row.OrganiserID,
			EventID:     row.EventID,
			OrderID:     row.OrderID,
			Kind:        domain.Kind(row.Kind),
			AmountCents: row.AmountCents,
			AvailableAt: row.AvailableAt,
			CreatedAt:   row.CreatedAt,
			Note:        row.Note,
		})
	}
	return domain.Page{Items: items, Total: total}, nil
}
