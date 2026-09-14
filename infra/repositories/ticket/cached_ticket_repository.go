package ticket

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	cachedomain "vozkot/domain/cache"
	domain "vozkot/domain/ticket"
)

// CachedTicketRepository is a cache-aside decorator over another repository.
//
// What it is allowed to be wrong about, and for how long, is the whole design:
//
//   - The listing a visitor reads may be up to TicketTTL stale. That is a
//     DISPLAY number. Whether a ticket can actually be taken is decided by the
//     conditional UPDATE in Reserve, which never reads this cache, so a stale
//     "4 left" can disappoint a buyer but can never oversell an event.
//   - Writes made through this decorator invalidate immediately. Writes made
//     inside a transaction go through the undecorated repository the unit of
//     work builds, so those are bounded by the TTL instead, which is why the
//     TTL is seconds rather than minutes.
type CachedTicketRepository struct {
	inner domain.Repository
	cache cachedomain.Cache
	ttl   time.Duration
}

var _ domain.Repository = (*CachedTicketRepository)(nil)

// TicketTTL is short on purpose: it is the longest an availability number may
// disagree with the database.
const TicketTTL = 3 * time.Second

func NewCachedTicketRepository(inner domain.Repository, cache cachedomain.Cache, ttl time.Duration) *CachedTicketRepository {
	if ttl <= 0 {
		ttl = TicketTTL
	}
	return &CachedTicketRepository{inner: inner, cache: cache, ttl: ttl}
}

func (r *CachedTicketRepository) GetByID(ctx context.Context, id string) (*domain.Ticket, error) {
	key := cachedomain.TicketKey(id)
	if raw, err := r.cache.Get(ctx, key); err == nil {
		var item domain.Ticket
		if json.Unmarshal(raw, &item) == nil {
			return &item, nil
		}
	}

	item, err := r.inner.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	r.store(ctx, key, item)
	return item, nil
}

func (r *CachedTicketRepository) List(ctx context.Context, filter domain.Filter) ([]domain.Ticket, error) {
	key := listKey(filter)
	if raw, err := r.cache.Get(ctx, key); err == nil {
		var items []domain.Ticket
		if json.Unmarshal(raw, &items) == nil {
			return items, nil
		}
	}

	items, err := r.inner.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	r.store(ctx, key, items)
	return items, nil
}

func (r *CachedTicketRepository) Count(ctx context.Context, filter domain.Filter) (int64, error) {
	// Deliberately uncached: it is one indexed count, and caching it separately
	// from the page it describes is how a total and a list start disagreeing.
	return r.inner.Count(ctx, filter)
}

func (r *CachedTicketRepository) Create(ctx context.Context, item *domain.Ticket) error {
	if err := r.inner.Create(ctx, item); err != nil {
		return err
	}
	r.invalidate(ctx, item.ID)
	return nil
}

func (r *CachedTicketRepository) Update(ctx context.Context, item *domain.Ticket) error {
	if err := r.inner.Update(ctx, item); err != nil {
		return err
	}
	r.invalidate(ctx, item.ID)
	return nil
}

func (r *CachedTicketRepository) Delete(ctx context.Context, id string) error {
	if err := r.inner.Delete(ctx, id); err != nil {
		return err
	}
	r.invalidate(ctx, id)
	return nil
}

func (r *CachedTicketRepository) Reserve(ctx context.Context, ticketID string, quantity int) (bool, error) {
	reserved, err := r.inner.Reserve(ctx, ticketID, quantity)
	if reserved {
		r.invalidate(ctx, ticketID)
	}
	return reserved, err
}

func (r *CachedTicketRepository) Release(ctx context.Context, ticketID string, quantity int) error {
	if err := r.inner.Release(ctx, ticketID, quantity); err != nil {
		return err
	}
	r.invalidate(ctx, ticketID)
	return nil
}

func (r *CachedTicketRepository) Commit(ctx context.Context, ticketID string, quantity int) error {
	if err := r.inner.Commit(ctx, ticketID, quantity); err != nil {
		return err
	}
	r.invalidate(ctx, ticketID)
	return nil
}

func (r *CachedTicketRepository) ReleaseSold(ctx context.Context, ticketID string, quantity int) error {
	if err := r.inner.ReleaseSold(ctx, ticketID, quantity); err != nil {
		return err
	}
	r.invalidate(ctx, ticketID)
	return nil
}

func (r *CachedTicketRepository) store(ctx context.Context, key string, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	if err := r.cache.Set(ctx, key, encoded, r.ttl); err != nil {
		// A cache that cannot be written is a slow system, not a broken one.
		log.Printf("cache: store %s: %v", key, err)
	}
}

func (r *CachedTicketRepository) invalidate(ctx context.Context, id string) {
	if err := r.cache.Delete(ctx, cachedomain.TicketKey(id)); err != nil {
		log.Printf("cache: invalidate %s: %v", id, err)
	}
	// Every listing page may contain this ticket, and none of them can be
	// identified cheaply, so the family goes.
	if err := r.cache.DeleteByPrefix(ctx, cachedomain.TicketListPrefix); err != nil {
		log.Printf("cache: invalidate listings: %v", err)
	}
}

// listKey encodes the whole filter, because two different filters are two
// different answers.
func listKey(filter domain.Filter) string {
	return fmt.Sprintf("%s%s|%s|%s|%s|%d|%d",
		cachedomain.TicketListPrefix,
		filter.Status, filter.Query, filter.OwnerID, filter.Sort, filter.Limit, filter.Offset)
}
