// Package cache is the read-path accelerator and the front door's bouncer.
//
// Two ports, because they are two different jobs:
//
//   - Cache absorbs reads. During an on-sale, every buyer polls their order for
//     a PIX code and every visitor reloads the same listing; those are the two
//     hottest queries in the system and neither needs to be answered from
//     PostgreSQL every time.
//   - RateLimiter absorbs abuse. A flash sale is indistinguishable from an
//     attack at the network layer, and the difference is made by counting.
//
// Nothing in here is allowed to become a source of truth. Inventory is decided
// by the conditional UPDATE in the database; a cached availability number is a
// display value that may be a second stale, and is invalidated the moment stock
// moves.
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrMiss means the key is absent. It is a normal outcome, never a failure.
var ErrMiss = errors.New("cache miss")

// Cache is a keyed byte store with expiry.
//
// Byte slices rather than a typed API: the caller owns the encoding, so a
// cached shape changing does not mean a cache adapter changing.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error
	// DeleteByPrefix drops a whole family of keys, which is how a listing is
	// invalidated when one row underneath it changes.
	DeleteByPrefix(ctx context.Context, prefix string) error
}

// RateLimiter counts events in a window.
//
// Allow reports whether this event may proceed, and how long to wait if not, so
// the HTTP layer can answer with a Retry-After instead of a bare refusal.
type RateLimiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (Decision, error)
}

// Decision is the outcome of one rate-limit check.
type Decision struct {
	Allowed bool
	// Remaining is how many more events fit in the current window.
	Remaining int
	// RetryAfter is how long until the window frees up. Zero when allowed.
	RetryAfter time.Duration
}

// Keys. Centralised so an invalidation and the read it invalidates cannot
// drift apart; the classic cache bug is two string literals that used to
// match.
const (
	// TicketListPrefix covers every cached listing page, whatever its filters.
	TicketListPrefix = "tickets:list:"
	// TicketPrefix covers single tickets.
	TicketPrefix = "tickets:item:"
	// OrderPrefix covers single orders, the key a checkout screen polls.
	OrderPrefix = "orders:item:"
	// SessionPrefix covers "is this access token's session still live", the
	// lookup the authentication middleware makes on EVERY authenticated
	// request, the busiest query in the system once buyers start polling.
	SessionPrefix = "sessions:jti:"
	// SessionLivePrefix maps a session id back to the key above.
	//
	// Revoking and rotating name a session by id, while the hot lookup is keyed
	// by the access token's JTI. Without this pointer a logout could not find
	// the entry it has to drop, and the session would stay live until the entry
	// expired on its own.
	SessionLivePrefix = "sessions:live:"
)

func TicketKey(id string) string { return TicketPrefix + id }

func OrderKey(id string) string { return OrderPrefix + id }

// SessionKey names the liveness entry for one access token. Both the account
// and the token id are in the key, so an entry can never answer for a different
// account even if a JTI were somehow reused.
func SessionKey(userID, jti string) string { return SessionPrefix + userID + ":" + jti }

func SessionLiveKey(sessionID string) string { return SessionLivePrefix + sessionID }
