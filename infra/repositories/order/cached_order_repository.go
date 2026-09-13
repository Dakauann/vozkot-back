package order

import (
	"context"
	"encoding/json"
	"log"
	"time"

	cachedomain "vozkot/domain/cache"
	domain "vozkot/domain/order"
)

// CachedOrderRepository caches the one read that dominates an on-sale: a buyer
// polling their own order while they wait for a PIX code.
//
// A checkout screen asks for the same row every second or two, per buyer, for
// as long as the hold lasts. Multiplied by a full house, that single query is
// the busiest in the system and it answers the same bytes nearly every time.
//
// The TTL is one second. Long enough to collapse a crowd of pollers onto one
// database read, short enough that the moment a payment lands the buyer sees it
// — and settlement invalidates the key anyway, so the second is a ceiling on
// the worst case, not the normal wait.
type CachedOrderRepository struct {
	inner domain.Repository
	cache cachedomain.Cache
	ttl   time.Duration
}

var _ domain.Repository = (*CachedOrderRepository)(nil)

const OrderTTL = time.Second

func NewCachedOrderRepository(inner domain.Repository, cache cachedomain.Cache, ttl time.Duration) *CachedOrderRepository {
	if ttl <= 0 {
		ttl = OrderTTL
	}
	return &CachedOrderRepository{inner: inner, cache: cache, ttl: ttl}
}

func (r *CachedOrderRepository) GetByID(ctx context.Context, id string) (*domain.Order, error) {
	key := cachedomain.OrderKey(id)
	if raw, err := r.cache.Get(ctx, key); err == nil {
		var item domain.Order
		if json.Unmarshal(raw, &item) == nil {
			return &item, nil
		}
	}

	item, err := r.inner.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if encoded, err := json.Marshal(item); err == nil {
		if err := r.cache.Set(ctx, key, encoded, r.ttl); err != nil {
			log.Printf("cache: store order %s: %v", id, err)
		}
	}
	return item, nil
}

func (r *CachedOrderRepository) Create(ctx context.Context, item *domain.Order) error {
	if err := r.inner.Create(ctx, item); err != nil {
		return err
	}
	r.invalidate(ctx, item.ID)
	return nil
}

func (r *CachedOrderRepository) Update(ctx context.Context, item *domain.Order) error {
	if err := r.inner.Update(ctx, item); err != nil {
		return err
	}
	r.invalidate(ctx, item.ID)
	return nil
}

func (r *CachedOrderRepository) ClaimExpired(ctx context.Context, now time.Time, limit int) ([]domain.Order, error) {
	claimed, err := r.inner.ClaimExpired(ctx, now, limit)
	for index := range claimed {
		r.invalidate(ctx, claimed[index].ID)
	}
	return claimed, err
}

// The remaining reads are uncached on purpose: a listing is paged and filtered
// per operator, and a lookup by idempotency key or payment id happens once per
// request on a unique index.

// GetByIDForUpdate never reads the cache: a locking read exists to see the
// truth the transaction is about to change.
func (r *CachedOrderRepository) GetByIDForUpdate(ctx context.Context, id string) (*domain.Order, error) {
	return r.inner.GetByIDForUpdate(ctx, id)
}

func (r *CachedOrderRepository) FindByIdempotencyKey(ctx context.Context, key string) (*domain.Order, error) {
	return r.inner.FindByIdempotencyKey(ctx, key)
}

func (r *CachedOrderRepository) FindByPaymentID(ctx context.Context, provider, paymentID string) (*domain.Order, error) {
	return r.inner.FindByPaymentID(ctx, provider, paymentID)
}

func (r *CachedOrderRepository) List(ctx context.Context, filter domain.Filter) ([]domain.Order, error) {
	return r.inner.List(ctx, filter)
}

func (r *CachedOrderRepository) Count(ctx context.Context, filter domain.Filter) (int64, error) {
	return r.inner.Count(ctx, filter)
}

func (r *CachedOrderRepository) invalidate(ctx context.Context, id string) {
	if err := r.cache.Delete(ctx, cachedomain.OrderKey(id)); err != nil {
		log.Printf("cache: invalidate order %s: %v", id, err)
	}
}
