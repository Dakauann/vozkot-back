package redis

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	domain "vozkot/domain/cache"
)

// RateLimiter is a fixed-window counter evaluated inside Redis.
//
// The script matters more than it looks. INCR and EXPIRE issued separately from
// Go is the classic broken limiter: between the two calls the process can die,
// leaving a counter with no expiry that refuses every future request for that
// key forever. A script runs both atomically on the server, so the key either
// exists with a TTL or does not exist.
//
// Fixed window rather than a sliding log because of what is being defended: a
// ticket on-sale is a thundering herd of legitimate traffic, and the cheapest
// counter that keeps one client from taking the whole queue is enough. A
// sliding window costs a sorted set per client for accuracy nobody here reads.
type RateLimiter struct {
	client *redis.Client
	prefix string
}

var _ domain.RateLimiter = (*RateLimiter)(nil)

func NewRateLimiter(cache *Cache) *RateLimiter {
	return &RateLimiter{client: cache.Client(), prefix: cache.prefix + ":rate:"}
}

// allowScript increments the window counter, setting the expiry only when the
// key is new, and returns the count plus the remaining TTL in milliseconds.
var allowScript = redis.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
local ttl = redis.call('PTTL', KEYS[1])
return {current, ttl}
`)

func (r *RateLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (domain.Decision, error) {
	if limit <= 0 {
		return domain.Decision{Allowed: true}, nil
	}
	if window <= 0 {
		window = time.Minute
	}

	result, err := allowScript.Run(ctx, r.client, []string{r.prefix + key}, window.Milliseconds()).Result()
	if err != nil {
		// A rate limiter that is down must not become a gate that is shut: the
		// product keeps working, and the log says the ceiling is off.
		return domain.Decision{Allowed: true}, err
	}

	values, ok := result.([]any)
	if !ok || len(values) < 2 {
		return domain.Decision{Allowed: true}, nil
	}
	current, _ := values[0].(int64)
	ttlMillis, _ := values[1].(int64)

	remaining := limit - int(current)
	if remaining < 0 {
		remaining = 0
	}
	if current <= int64(limit) {
		return domain.Decision{Allowed: true, Remaining: remaining}, nil
	}

	retryAfter := time.Duration(ttlMillis) * time.Millisecond
	if retryAfter < 0 {
		retryAfter = window
	}
	return domain.Decision{Allowed: false, Remaining: 0, RetryAfter: retryAfter}, nil
}
