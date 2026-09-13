package order

import (
	"context"
	"errors"
	"strings"
	"time"

	domain "vozkot/domain/order"
	"vozkot/domain/payment"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type OrderRepository struct {
	db *gorm.DB
}

func NewOrderRepository(db *gorm.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

func (r *OrderRepository) Create(ctx context.Context, item *domain.Order) error {
	record := toSchema(item)
	result := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		// The unique keys on this table are the id, the idempotency key and the
		// provider payment id, and each means "this already exists". Decided by
		// the database without an error: a buyer whose retry lost the race is
		// ordinary traffic, not a failed statement to log.
		return domain.ErrIdempotencyMismatch
	}
	*item = *toDomain(&record)
	return nil
}

func (r *OrderRepository) GetByID(ctx context.Context, id string) (*domain.Order, error) {
	var record schema.Order
	if err := r.db.WithContext(ctx).First(&record, "id = ?", id).Error; err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

// GetByIDForUpdate takes the row lock a settlement needs.
//
// SELECT ... FOR UPDATE, not a Go mutex: the two concurrent settlements may be
// on different machines, and the only lock they share is the database's.
func (r *OrderRepository) GetByIDForUpdate(ctx context.Context, id string) (*domain.Order, error) {
	var record schema.Order
	if err := r.db.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&record, "id = ?", id).Error; err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

func (r *OrderRepository) FindByIdempotencyKey(ctx context.Context, key string) (*domain.Order, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, domain.ErrNotFound
	}
	var record schema.Order
	if err := r.db.WithContext(ctx).First(&record, "idempotency_key = ?", key).Error; err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

func (r *OrderRepository) FindByPaymentID(ctx context.Context, provider, paymentID string) (*domain.Order, error) {
	paymentID = strings.TrimSpace(paymentID)
	if paymentID == "" {
		return nil, domain.ErrNotFound
	}
	var record schema.Order
	query := r.db.WithContext(ctx).Where("payment_id = ?", paymentID)
	if provider = strings.TrimSpace(provider); provider != "" {
		query = query.Where("payment_provider = ?", provider)
	}
	if err := query.First(&record).Error; err != nil {
		return nil, translate(err)
	}
	return toDomain(&record), nil
}

func (r *OrderRepository) List(ctx context.Context, filter domain.Filter) ([]domain.Order, error) {
	var records []schema.Order
	query := applyFilter(r.db.WithContext(ctx).Model(&schema.Order{}), filter)
	if filter.OldestUpdatedFirst {
		query = query.Order("updated_at ASC, id ASC")
	} else {
		query = query.Order("created_at DESC")
	}
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		query = query.Offset(filter.Offset)
	}
	if err := query.Find(&records).Error; err != nil {
		return nil, err
	}
	items := make([]domain.Order, 0, len(records))
	for index := range records {
		items = append(items, *toDomain(&records[index]))
	}
	return items, nil
}

func (r *OrderRepository) Count(ctx context.Context, filter domain.Filter) (int64, error) {
	var total int64
	if err := applyFilter(r.db.WithContext(ctx).Model(&schema.Order{}), filter).Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func (r *OrderRepository) Update(ctx context.Context, item *domain.Order) error {
	record := toSchema(item)
	result := r.db.WithContext(ctx).Model(&schema.Order{}).Where("id = ?", record.ID).Updates(map[string]any{
		"status":             record.Status,
		"hold_expires_at":    record.HoldExpiresAt,
		"payment_provider":   record.PaymentProvider,
		"payment_id":         record.PaymentID,
		"payment_status":     record.PaymentStatus,
		"payment_method":     record.PaymentMethod,
		"pix_copy_paste":     record.PixCopyPaste,
		"pix_qr_code_base64": record.PixQRCodeBase64,
		"paid_at":            record.PaidAt,
		"closed_at":          record.ClosedAt,
		"updated_at":         record.UpdatedAt,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ClaimExpired flips lapsed holds to expired and returns exactly the rows it
// flipped.
//
// One statement, with SKIP LOCKED on the inner select: two sweepers running at
// the same time take disjoint sets, so the stock each releases is stock it
// personally took off the shelf. A read-then-update version would let both see
// the same orders and release the same tickets twice, which quietly inflates
// availability and oversells the event.
func (r *OrderRepository) ClaimExpired(ctx context.Context, now time.Time, limit int) ([]domain.Order, error) {
	if limit <= 0 {
		limit = 100
	}
	timestamp := now.UTC()

	var records []schema.Order
	err := r.db.WithContext(ctx).Raw(`
		UPDATE orders
		SET status = ?, closed_at = COALESCE(closed_at, ?), updated_at = ?
		WHERE id IN (
			SELECT id FROM orders
			WHERE status = ? AND hold_expires_at <= ?
			ORDER BY hold_expires_at
			LIMIT ?
			FOR UPDATE SKIP LOCKED
		)
		RETURNING *`,
		string(domain.StatusExpired), timestamp, timestamp,
		string(domain.StatusPendingPayment), timestamp, limit,
	).Scan(&records).Error
	if err != nil {
		return nil, err
	}

	items := make([]domain.Order, 0, len(records))
	for index := range records {
		items = append(items, *toDomain(&records[index]))
	}
	return items, nil
}

func applyFilter(query *gorm.DB, filter domain.Filter) *gorm.DB {
	if filter.TicketID != "" {
		query = query.Where("ticket_id = ?", filter.TicketID)
	}
	if filter.BuyerID != "" {
		query = query.Where("buyer_id = ?", filter.BuyerID)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", string(filter.Status))
	}
	return query
}

func translate(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrNotFound
	}
	return err
}

func toSchema(item *domain.Order) schema.Order {
	var key *string
	if trimmed := strings.TrimSpace(item.IdempotencyKey); trimmed != "" {
		key = &trimmed
	}
	return schema.Order{
		ID:              item.ID,
		TicketID:        item.TicketID,
		BuyerID:         item.BuyerID,
		BuyerName:       item.BuyerName,
		BuyerEmail:      item.BuyerEmail,
		BuyerDocument:   item.BuyerDocument,
		Quantity:        item.Quantity,
		UnitPriceCents:  item.UnitPriceCents,
		TotalCents:      item.TotalCents,
		Currency:        item.Currency,
		Status:          string(item.Status),
		HoldExpiresAt:   item.HoldExpiresAt,
		PaymentProvider: string(item.PaymentProvider),
		PaymentID:       item.PaymentID,
		PaymentStatus:   string(item.PaymentStatus),
		PaymentMethod:   string(item.PaymentMethod),
		PixCopyPaste:    item.PixCopyPaste,
		PixQRCodeBase64: item.PixQRCodeBase64,
		IdempotencyKey:  key,
		PaidAt:          item.PaidAt,
		ClosedAt:        item.ClosedAt,
		CreatedAt:       item.CreatedAt,
		UpdatedAt:       item.UpdatedAt,
	}
}

func toDomain(record *schema.Order) *domain.Order {
	key := ""
	if record.IdempotencyKey != nil {
		key = *record.IdempotencyKey
	}
	return &domain.Order{
		ID:              record.ID,
		TicketID:        record.TicketID,
		BuyerID:         record.BuyerID,
		BuyerName:       record.BuyerName,
		BuyerEmail:      record.BuyerEmail,
		BuyerDocument:   record.BuyerDocument,
		Quantity:        record.Quantity,
		UnitPriceCents:  record.UnitPriceCents,
		TotalCents:      record.TotalCents,
		Currency:        record.Currency,
		Status:          domain.Status(record.Status),
		HoldExpiresAt:   record.HoldExpiresAt,
		PaymentProvider: payment.Provider(record.PaymentProvider),
		PaymentID:       record.PaymentID,
		PaymentStatus:   payment.Status(record.PaymentStatus),
		PaymentMethod:   payment.Method(record.PaymentMethod),
		PixCopyPaste:    record.PixCopyPaste,
		PixQRCodeBase64: record.PixQRCodeBase64,
		IdempotencyKey:  key,
		PaidAt:          record.PaidAt,
		ClosedAt:        record.ClosedAt,
		CreatedAt:       record.CreatedAt,
		UpdatedAt:       record.UpdatedAt,
	}
}
