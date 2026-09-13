package redis_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	domain "vozkot/domain/cache"
	"vozkot/infra/redis"
	"vozkot/infra/testsupport"
)

// Runs against Redis. The cache is a keyed store with expiry and the limiter is
// a Lua script's atomicity; neither is worth testing against anything else.

func TestCacheRoundTripAndExpiry(t *testing.T) {
	cache := testsupport.Cache(t)
	ctx := context.Background()

	if _, err := cache.Get(ctx, "missing"); !errors.Is(err, domain.ErrMiss) {
		t.Fatalf("Get(missing) error = %v, want %v", err, domain.ErrMiss)
	}

	if err := cache.Set(ctx, "greeting", []byte("olá"), time.Second); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	value, err := cache.Get(ctx, "greeting")
	if err != nil || string(value) != "olá" {
		t.Fatalf("Get() = (%q, %v)", value, err)
	}

	// A TTL is a promise the entry disappears: an availability number cached
	// forever is an oversold event waiting to be displayed.
	time.Sleep(1200 * time.Millisecond)
	if _, err := cache.Get(ctx, "greeting"); !errors.Is(err, domain.ErrMiss) {
		t.Fatalf("Get() after TTL error = %v, want %v", err, domain.ErrMiss)
	}
}

func TestDeleteByPrefixOnlyTouchesTheFamily(t *testing.T) {
	cache := testsupport.Cache(t)
	ctx := context.Background()

	for _, key := range []string{"tickets:list:a", "tickets:list:b", "tickets:item:1", "orders:item:1"} {
		if err := cache.Set(ctx, key, []byte("x"), time.Minute); err != nil {
			t.Fatalf("Set(%s) error = %v", key, err)
		}
	}

	if err := cache.DeleteByPrefix(ctx, "tickets:list:"); err != nil {
		t.Fatalf("DeleteByPrefix() error = %v", err)
	}

	for _, gone := range []string{"tickets:list:a", "tickets:list:b"} {
		if _, err := cache.Get(ctx, gone); !errors.Is(err, domain.ErrMiss) {
			t.Fatalf("%s survived the prefix delete", gone)
		}
	}
	for _, kept := range []string{"tickets:item:1", "orders:item:1"} {
		if _, err := cache.Get(ctx, kept); err != nil {
			t.Fatalf("%s was deleted by a prefix it does not share", kept)
		}
	}
}

func TestRateLimiterCountsAWindowAtomically(t *testing.T) {
	cache := testsupport.Cache(t)
	limiter := redis.NewRateLimiter(cache)
	ctx := context.Background()
	key := testsupport.Unique("buyer")

	// Fifty callers hammer one key at once with a limit of ten. Exactly ten
	// pass: the INCR and the EXPIRE are one script, so no interleaving can
	// lose a count or leave a counter without a TTL.
	var wait sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 50; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			decision, err := limiter.Allow(ctx, key, 10, time.Minute)
			if err != nil {
				t.Errorf("Allow() error = %v", err)
				return
			}
			if decision.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()

	if allowed != 10 {
		t.Fatalf("allowed = %d, want exactly 10", allowed)
	}

	refused, err := limiter.Allow(ctx, key, 10, time.Minute)
	if err != nil || refused.Allowed {
		t.Fatalf("Allow() after the limit = (%+v, %v), want refused", refused, err)
	}
	if refused.RetryAfter <= 0 || refused.RetryAfter > time.Minute {
		t.Fatalf("RetryAfter = %s, want a value inside the window so a client can back off", refused.RetryAfter)
	}
}

func TestRateLimiterWindowResets(t *testing.T) {
	cache := testsupport.Cache(t)
	limiter := redis.NewRateLimiter(cache)
	ctx := context.Background()
	key := testsupport.Unique("buyer")

	for i := 0; i < 2; i++ {
		if decision, _ := limiter.Allow(ctx, key, 2, time.Second); !decision.Allowed {
			t.Fatalf("call %d refused inside the limit", i+1)
		}
	}
	if decision, _ := limiter.Allow(ctx, key, 2, time.Second); decision.Allowed {
		t.Fatal("third call allowed with a limit of two")
	}

	time.Sleep(1200 * time.Millisecond)
	if decision, _ := limiter.Allow(ctx, key, 2, time.Second); !decision.Allowed {
		t.Fatal("the window did not reset after its TTL")
	}
}
