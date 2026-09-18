package refund

import (
	"context"
	"errors"
	"strings"

	domain "vozkot/domain/refund"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RefundRepository struct {
	db *gorm.DB
}

func NewRefundRepository(db *gorm.DB) *RefundRepository {
	return &RefundRepository{db: db}
}

var _ domain.Repository = (*RefundRepository)(nil)

// Create writes a request and lets the DATABASE refuse a second open one.
//
// The partial unique index on (order_id) WHERE status IN ('pending','approved')
// is the authority. A prior SELECT would be decoration: a buyer double-tapping
// "cancelar" sends two requests that both read "nothing open" and both insert,
// which is two refunds for one order, the one failure this whole feature must
// not have. The conflict comes back as ErrAlreadyOpen, which is ordinary
// traffic rather than an error to log.
func (r *RefundRepository) Create(ctx context.Context, item *domain.Request) error {
	record := toSchema(item)
	if err := r.db.WithContext(ctx).Omit(clause.Associations).Create(&record).Error; err != nil {
		if isOpenConflict(err) {
			return domain.ErrAlreadyOpen
		}
		return err
	}
	*item = *toDomain(&record)
	return nil
}

func (r *RefundRepository) GetByID(ctx context.Context, id string) (*domain.Request, error) {
	var record schema.RefundRequest
	if err := r.db.WithContext(ctx).First(&record, "id = ?", strings.TrimSpace(id)).Error; err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

// GetByIDForUpdate holds the row for the rest of the transaction, so two people
// pressing approve and reject at the same moment take turns rather than one
// overwriting the other's answer.
func (r *RefundRepository) GetByIDForUpdate(ctx context.Context, id string) (*domain.Request, error) {
	var record schema.RefundRequest
	if err := r.db.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&record, "id = ?", strings.TrimSpace(id)).Error; err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

func (r *RefundRepository) FindOpenByOrder(ctx context.Context, orderID string) (*domain.Request, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return nil, domain.ErrNotFound
	}
	var record schema.RefundRequest
	err := r.db.WithContext(ctx).
		Where("order_id = ? AND status IN ?", orderID, openStatuses()).
		Order("created_at DESC").
		First(&record).Error
	if err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

func (r *RefundRepository) List(ctx context.Context, filter domain.Filter) (domain.Page, error) {
	query := applyFilter(r.db.WithContext(ctx).Model(&schema.RefundRequest{}), filter)

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return domain.Page{}, err
	}

	// Pending first, then newest: an inbox is read to find what is waiting on
	// you, and a decided request is history. Ordering by status alone would be
	// arbitrary, so the status is mapped to a rank in SQL rather than sorted
	// alphabetically, where "approved" would come before "pending".
	query = query.Order(`
		CASE status WHEN 'pending' THEN 0 WHEN 'approved' THEN 1 ELSE 2 END,
		created_at DESC`)
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		query = query.Offset(filter.Offset)
	}

	var records []schema.RefundRequest
	if err := query.Find(&records).Error; err != nil {
		return domain.Page{}, err
	}
	items := make([]domain.Request, 0, len(records))
	for index := range records {
		items = append(items, *toDomain(&records[index]))
	}
	return domain.Page{Items: items, Total: total}, nil
}

// Update writes back only what a decision changes.
//
// The order, the grounds and the amount are absent on purpose: they are what
// the request WAS, and a path that could rewrite them would be a path that
// could quietly change what somebody approved after they approved it.
func (r *RefundRepository) Update(ctx context.Context, item *domain.Request) error {
	record := toSchema(item)
	result := r.db.WithContext(ctx).Model(&schema.RefundRequest{}).
		Where("id = ?", record.ID).
		Updates(map[string]any{
			"status":        record.Status,
			"decided_by":    record.DecidedBy,
			"decision_note": record.DecisionNote,
			"decided_at":    record.DecidedAt,
			"updated_at":    record.UpdatedAt,
		})
	if result.Error != nil {
		if isOpenConflict(result.Error) {
			return domain.ErrAlreadyOpen
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// OpenByOrders answers "which of these orders already have a request" for a
// whole page in one query.
//
// The orders listing renders a refund button per row, and asking per row is the
// N+1 that turns a buyer's order history into twenty-one round trips.
func (r *RefundRepository) OpenByOrders(ctx context.Context, orderIDs []string) (map[string]domain.Request, error) {
	found := make(map[string]domain.Request, len(orderIDs))
	if len(orderIDs) == 0 {
		return found, nil
	}
	var records []schema.RefundRequest
	err := r.db.WithContext(ctx).
		Where("order_id IN ? AND status IN ?", orderIDs, openStatuses()).
		Order("created_at DESC").
		Find(&records).Error
	if err != nil {
		return nil, err
	}
	for index := range records {
		record := records[index]
		// Newest wins, and the partial index means there is only ever one.
		if _, already := found[record.OrderID]; already {
			continue
		}
		found[record.OrderID] = *toDomain(&record)
	}
	return found, nil
}

// openStatuses is the set the partial unique index is defined over. Kept in one
// place so the index, the lookups and the domain's own Open() cannot drift.
func openStatuses() []string {
	return []string{string(domain.StatusPending), string(domain.StatusApproved)}
}

func applyFilter(query *gorm.DB, filter domain.Filter) *gorm.DB {
	if filter.EventID != "" {
		query = query.Where("event_id = ?", filter.EventID)
	}
	if filter.BuyerID != "" {
		query = query.Where("buyer_id = ?", filter.BuyerID)
	}
	if filter.OrderID != "" {
		query = query.Where("order_id = ?", filter.OrderID)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", string(filter.Status))
	}
	if filter.Open {
		query = query.Where("status IN ?", openStatuses())
	}
	return query
}

// isOpenConflict recognises the partial unique index losing a race.
//
// Matched on the constraint NAME as well as on GORM's translated error, so a
// PostgreSQL upgrade that rewords its messages turns a duplicate request into
// the right refusal rather than a 500.
func isOpenConflict(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	return strings.Contains(err.Error(), "idx_refund_requests_open")
}

func translate(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrNotFound
	}
	return err
}

func toSchema(item *domain.Request) schema.RefundRequest {
	return schema.RefundRequest{
		ID:           item.ID,
		OrderID:      item.OrderID,
		EventID:      item.EventID,
		BuyerID:      item.BuyerID,
		Reason:       string(item.Reason),
		Status:       string(item.Status),
		AmountCents:  item.AmountCents,
		FeeCents:     item.FeeCents,
		RequestedBy:  item.RequestedBy,
		Note:         item.Note,
		DecidedBy:    item.DecidedBy,
		DecisionNote: item.DecisionNote,
		DecidedAt:    item.DecidedAt,
		CreatedAt:    item.CreatedAt,
		UpdatedAt:    item.UpdatedAt,
	}
}

func toDomain(record *schema.RefundRequest) *domain.Request {
	return &domain.Request{
		ID:           record.ID,
		OrderID:      record.OrderID,
		EventID:      record.EventID,
		BuyerID:      record.BuyerID,
		Reason:       domain.Reason(record.Reason),
		Status:       domain.Status(record.Status),
		AmountCents:  record.AmountCents,
		FeeCents:     record.FeeCents,
		RequestedBy:  record.RequestedBy,
		Note:         record.Note,
		DecidedBy:    record.DecidedBy,
		DecisionNote: record.DecisionNote,
		DecidedAt:    record.DecidedAt,
		CreatedAt:    record.CreatedAt,
		UpdatedAt:    record.UpdatedAt,
	}
}
