package order

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
	"time"

	domain "vozkot/domain/order"
	"vozkot/domain/payment"
	seatingdomain "vozkot/domain/seating"
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

// Create writes the order and its lines.
//
// Both, or neither. The caller is always inside a unit of work, checkout
// reserves stock in the same transaction, so an order that committed without
// its items is not a state this can reach. The items are written with their own
// conflict clause because a retry that got as far as the lines before dying
// must be able to finish rather than fail on rows it wrote itself.
func (r *OrderRepository) Create(ctx context.Context, item *domain.Order) error {
	record := toSchema(item)
	result := r.db.WithContext(ctx).
		Omit(clause.Associations).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&record)
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

	lines := itemsToSchema(item)
	if len(lines) > 0 {
		if err := r.db.WithContext(ctx).
			Clauses(clause.OnConflict{DoNothing: true}).
			Create(&lines).Error; err != nil {
			return err
		}
	}

	stored := toDomain(&record)
	stored.Items = item.Items
	*item = *stored
	return nil
}

func (r *OrderRepository) GetByID(ctx context.Context, id string) (*domain.Order, error) {
	var record schema.Order
	if err := r.db.WithContext(ctx).First(&record, "id = ?", id).Error; err != nil {
		return nil, translate(err)
	}
	item := toDomain(&record)
	if err := r.attachItems(ctx, []*domain.Order{item}); err != nil {
		return nil, err
	}
	return item, nil
}

// GetByIDForUpdate takes the row lock a settlement needs.
//
// SELECT ... FOR UPDATE, not a Go mutex: the two concurrent settlements may be
// on different machines, and the only lock they share is the database's.
//
// The lock is on the order row alone. The lines are immutable once written;
// nothing in the system ever updates an order_item, so reading them outside
// the lock cannot see a half-changed set.
func (r *OrderRepository) GetByIDForUpdate(ctx context.Context, id string) (*domain.Order, error) {
	var record schema.Order
	if err := r.db.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		First(&record, "id = ?", id).Error; err != nil {
		return nil, translate(err)
	}
	item := toDomain(&record)
	if err := r.attachItems(ctx, []*domain.Order{item}); err != nil {
		return nil, err
	}
	return item, nil
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
	item := toDomain(&record)
	if err := r.attachItems(ctx, []*domain.Order{item}); err != nil {
		return nil, err
	}
	return item, nil
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
	item := toDomain(&record)
	if err := r.attachItems(ctx, []*domain.Order{item}); err != nil {
		return nil, err
	}
	return item, nil
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
	// One extra query for the whole page rather than one per order. A listing
	// of twenty orders is the dashboard's normal request, and the N+1 version
	// of it is twenty-one round trips for a screen.
	pointers := make([]*domain.Order, 0, len(items))
	for index := range items {
		pointers = append(pointers, &items[index])
	}
	if err := r.attachItems(ctx, pointers); err != nil {
		return nil, err
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

// Update writes back only what can change after an order exists.
//
// The items are absent on purpose. What was bought is settled at checkout, and
// a path that could rewrite a line would be a path that could rewrite what
// somebody paid for.
func (r *OrderRepository) Update(ctx context.Context, item *domain.Order) error {
	record := toSchema(item)
	result := r.db.WithContext(ctx).Model(&schema.Order{}).Where("id = ?", record.ID).Updates(map[string]any{
		"buyer_name":         record.BuyerName,
		"buyer_email":        record.BuyerEmail,
		"buyer_document":     record.BuyerDocument,
		"status":             record.Status,
		"hold_expires_at":    record.HoldExpiresAt,
		"confirmed":          record.Confirmed,
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
	// RETURNING gives back the order rows and nothing else, and the caller is
	// about to release stock against every line. Loading them is not an
	// optimisation here, without the items the sweep would release nothing.
	pointers := make([]*domain.Order, 0, len(items))
	for index := range items {
		pointers = append(pointers, &items[index])
	}
	if err := r.attachItems(ctx, pointers); err != nil {
		return nil, err
	}
	return items, nil
}

// buyerHoldLockNamespace keeps this lock family to itself.
//
// PostgreSQL's two-key advisory locks live in a separate space from the
// single-key ones, so the migration lock is already out of reach; the namespace
// is here so a second two-key lock added later cannot collide with this one by
// picking the same hash.
const buyerHoldLockNamespace int32 = 21781

// CountOpenHoldsForUpdate serialises one buyer's concurrent checkouts, then
// counts what they are already holding.
//
// The lock comes first and it is the entire point. Two checkouts by one account
// arriving together would both read "one open order", both pass a limit of
// three, and both commit: a limit that only holds when nobody tries. A
// transaction-scoped advisory lock keyed on the buyer makes the count and the
// INSERT it guards one indivisible step, and it releases itself at commit or
// rollback, so no path can leak it.
//
// Keyed on a hash of the buyer id, so two different accounts may occasionally
// share a lock. That costs the unlucky pair one short wait and nothing else:
// the hash is never used to decide who holds what, only to decide who waits.
//
// The per-tier counts come back as a map covering the tiers asked about. An
// order now spans several tiers, and a cap that checked only the first of them
// would be a cap the buyer chooses the strength of by reordering their basket.
func (r *OrderRepository) CountOpenHoldsForUpdate(
	ctx context.Context,
	buyerID string,
	ticketIDs []string,
) (domain.OpenHolds, error) {
	buyerID = strings.TrimSpace(buyerID)
	if buyerID == "" {
		// A sale with no account behind it, an operator at the door, has no
		// identity to count against, and nothing to serialise on.
		return domain.OpenHolds{}, nil
	}

	if err := r.db.WithContext(ctx).
		Exec(`SELECT pg_advisory_xact_lock(?::int4, ?::int4)`, buyerHoldLockNamespace, buyerLockKey(buyerID)).
		Error; err != nil {
		return domain.OpenHolds{}, err
	}

	holds := domain.OpenHolds{TicketsByTier: make(map[string]int, len(ticketIDs))}

	var openOrders int64
	if err := r.db.WithContext(ctx).Model(&schema.Order{}).
		Where("buyer_id = ? AND status = ?", buyerID, string(domain.StatusPendingPayment)).
		Count(&openOrders).Error; err != nil {
		return domain.OpenHolds{}, err
	}
	holds.Orders = int(openOrders)

	if len(ticketIDs) == 0 {
		return holds, nil
	}

	var counted []struct {
		TicketID string
		Quantity int
	}
	// Grouped in the database rather than summed in Go: the buyer may hold
	// dozens of lines across their open orders, and only the totals are wanted.
	err := r.db.WithContext(ctx).Raw(`
		SELECT i.ticket_id AS ticket_id, COALESCE(SUM(i.quantity), 0) AS quantity
		FROM order_items i
		JOIN orders o ON o.id = i.order_id
		WHERE o.buyer_id = ? AND o.status = ? AND i.ticket_id IN ?
		GROUP BY i.ticket_id`,
		buyerID, string(domain.StatusPendingPayment), ticketIDs,
	).Scan(&counted).Error
	if err != nil {
		return domain.OpenHolds{}, err
	}
	for _, row := range counted {
		holds.TicketsByTier[row.TicketID] = row.Quantity
	}
	return holds, nil
}

// attachItems loads the lines for a page of orders in one query.
func (r *OrderRepository) attachItems(ctx context.Context, orders []*domain.Order) error {
	if len(orders) == 0 {
		return nil
	}
	ids := make([]string, 0, len(orders))
	for _, item := range orders {
		ids = append(ids, item.ID)
	}

	var records []schema.OrderItem
	// Ordered by ticket id so an order's lines always come back in the same
	// sequence the domain sorted them into, which is the sequence the
	// reservation loop takes its locks in.
	if err := r.db.WithContext(ctx).
		Where("order_id IN ?", ids).
		Order("order_id, ticket_id").
		Find(&records).Error; err != nil {
		return err
	}

	grouped := make(map[string][]domain.Item, len(orders))
	for index := range records {
		record := records[index]
		grouped[record.OrderID] = append(grouped[record.OrderID], domain.Item{
			ID:             record.ID,
			OrderID:        record.OrderID,
			TicketID:       record.TicketID,
			TicketTitle:    record.TicketTitle,
			Quantity:       record.Quantity,
			UnitPriceCents: record.UnitPriceCents,
			TotalCents:     record.TotalCents,
			UnitFeeCents:   record.UnitFeeCents,
			FeeCents:       record.FeeCents,
			SeatID:         seatIDOf(record.SeatID),
			Seat: seatingdomain.Label{
				Section: record.SeatSection,
				Row:     record.SeatRow,
				Seat:    record.SeatLabel,
			},
			SeatKind: seatingdomain.SeatKind(record.SeatKind),
		})
	}
	for _, item := range orders {
		item.Items = grouped[item.ID]
	}
	return nil
}

// buyerLockKey folds a buyer id into the int32 an advisory lock takes.
//
// Computed here rather than with PostgreSQL's hashtext(), which is an
// undocumented internal whose value is not promised to be stable across major
// versions, and a lock key that changes under an upgrade is a lock that stops
// serialising during exactly the window nobody is watching.
func buyerLockKey(buyerID string) int32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(buyerID))
	return int32(hash.Sum32())
}

func applyFilter(query *gorm.DB, filter domain.Filter) *gorm.DB {
	if filter.TicketID != "" {
		// Through the lines, because a tier is no longer a column on the order.
		// EXISTS rather than a join: the line is not wanted, only the fact that
		// one names this tier, and a join would need de-duplicating.
		query = query.Where(`
			EXISTS (
				SELECT 1 FROM order_items
				WHERE order_items.order_id = orders.id AND order_items.ticket_id = ?
			)`, filter.TicketID)
	}
	if filter.EventID != "" {
		query = query.Where("orders.event_id = ?", filter.EventID)
	}
	if filter.BuyerID != "" {
		query = query.Where("buyer_id = ?", filter.BuyerID)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", string(filter.Status))
	}
	if len(filter.Statuses) > 0 {
		values := make([]string, 0, len(filter.Statuses))
		for _, status := range filter.Statuses {
			// Trimmed HERE, not only where the query string was parsed. This is
			// the layer that builds the SQL, and a blank that reaches it
			// becomes `IN ('')`, which matches nothing and tells a buyer they
			// have no orders at all. It must not depend on a caller upstream
			// having tidied the value first.
			if trimmed := strings.TrimSpace(string(status)); trimmed != "" {
				values = append(values, trimmed)
			}
		}
		// Only when something survived. A caller who passed nothing but blanks
		// asked for no constraint, not for no results.
		if len(values) > 0 {
			query = query.Where("status IN ?", values)
		}
	}
	if !filter.EventNotBefore.IsZero() {
		// Straight to the event the order names. It used to have to go through
		// the tier, because the order only knew a price; it now knows the
		// night.
		query = query.Where(`
			EXISTS (
				SELECT 1 FROM events
				WHERE events.id = orders.event_id AND events.starts_at >= ?
			)`, filter.EventNotBefore.UTC())
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
		EventID:         item.EventID,
		BuyerID:         item.BuyerID,
		BuyerName:       item.BuyerName,
		BuyerEmail:      item.BuyerEmail,
		BuyerDocument:   item.BuyerDocument,
		BuyerGender:     item.BuyerGender,
		BuyerAgeYears:   item.BuyerAgeYears,
		BuyerCity:       item.BuyerCity,
		BuyerUF:         item.BuyerUF,
		SubtotalCents:   item.SubtotalCents,
		ServiceFeeCents: item.BuyerFeeCents,
		TotalCents:      item.TotalCents,
		Currency:        item.Currency,

		RefundPolicyVersion: item.RefundPolicyVersion,
		Status:              string(item.Status),
		HoldExpiresAt:       item.HoldExpiresAt,
		Confirmed:           item.Confirmed,
		PaymentProvider:     string(item.PaymentProvider),
		PaymentID:           item.PaymentID,
		PaymentStatus:       string(item.PaymentStatus),
		PaymentMethod:       string(item.PaymentMethod),
		PixCopyPaste:        item.PixCopyPaste,
		PixQRCodeBase64:     item.PixQRCodeBase64,
		IdempotencyKey:      key,
		PaidAt:              item.PaidAt,
		ClosedAt:            item.ClosedAt,
		CreatedAt:           item.CreatedAt,
		UpdatedAt:           item.UpdatedAt,
	}
}

// seatIDColumn maps an empty seat id to SQL NULL; see OrderItem.SeatID.
func seatIDColumn(seatID string) *string {
	trimmed := strings.TrimSpace(seatID)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// seatIDOf is the read direction of seatIDColumn.
func seatIDOf(seatID *string) string {
	if seatID == nil {
		return ""
	}
	return *seatID
}

func itemsToSchema(item *domain.Order) []schema.OrderItem {
	lines := make([]schema.OrderItem, 0, len(item.Items))
	for _, line := range item.Items {
		lines = append(lines, schema.OrderItem{
			ID:             line.ID,
			OrderID:        item.ID,
			TicketID:       line.TicketID,
			TicketTitle:    line.TicketTitle,
			Quantity:       line.Quantity,
			UnitPriceCents: line.UnitPriceCents,
			TotalCents:     line.TotalCents,
			UnitFeeCents:   line.UnitFeeCents,
			FeeCents:       line.FeeCents,
			// NULL and not "" for a counted line: the partial unique indexes
			// on this table tell the two kinds of line apart by IS NULL, and an
			// empty string would make every counted line collide with the next.
			SeatID:      seatIDColumn(line.SeatID),
			SeatSection: line.Seat.Section,
			SeatRow:     line.Seat.Row,
			SeatLabel:   line.Seat.Seat,
			SeatKind:    string(line.SeatKind),
			CreatedAt:   item.CreatedAt,
		})
	}
	return lines
}

func toDomain(record *schema.Order) *domain.Order {
	key := ""
	if record.IdempotencyKey != nil {
		key = *record.IdempotencyKey
	}
	return &domain.Order{
		ID:            record.ID,
		EventID:       record.EventID,
		BuyerID:       record.BuyerID,
		BuyerName:     record.BuyerName,
		BuyerEmail:    record.BuyerEmail,
		BuyerDocument: record.BuyerDocument,
		BuyerGender:   record.BuyerGender,
		BuyerAgeYears: record.BuyerAgeYears,
		BuyerCity:     record.BuyerCity,
		BuyerUF:       record.BuyerUF,
		SubtotalCents: record.SubtotalCents,
		BuyerFeeCents: record.ServiceFeeCents,
		TotalCents:    record.TotalCents,
		Currency:      record.Currency,

		RefundPolicyVersion: record.RefundPolicyVersion,
		Status:              domain.Status(record.Status),
		HoldExpiresAt:       record.HoldExpiresAt,
		Confirmed:           record.Confirmed,
		PaymentProvider:     payment.Provider(record.PaymentProvider),
		PaymentID:           record.PaymentID,
		PaymentStatus:       payment.Status(record.PaymentStatus),
		PaymentMethod:       payment.Method(record.PaymentMethod),
		PixCopyPaste:        record.PixCopyPaste,
		PixQRCodeBase64:     record.PixQRCodeBase64,
		IdempotencyKey:      key,
		PaidAt:              record.PaidAt,
		ClosedAt:            record.ClosedAt,
		CreatedAt:           record.CreatedAt,
		UpdatedAt:           record.UpdatedAt,
	}
}
