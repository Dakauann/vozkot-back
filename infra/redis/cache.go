// Package redis is the cache and the rate limiter.
//
// Redis, not an in-process map, for one reason that decides it: this
// application runs as more than one instance. A local map would give each
// instance its own answer, each invalidation would reach one of them, and the
// rate limiter would permit N times the configured limit across N pods.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	domain "vozkot/domain/cache"
)

// Cache is a Redis-backed implementation of the cache port.
type Cache struct {
	client *redis.Client
	prefix string
}

var _ domain.Cache = (*Cache)(nil)

// Connect dials Redis and verifies it answers.
//
// The ping is deliberate: a cache that silently fails to connect degrades into
// a very slow no-op, and the symptom shows up as database load in a week rather
// than as an error at boot.
func Connect(ctx context.Context, url, prefix string) (*Cache, error) {
	options, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis: parse url: %w", err)
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("redis: ping: %w", err)
	}
	return &Cache{client: client, prefix: strings.TrimSuffix(strings.TrimSpace(prefix), ":")}, nil
}

// Client exposes the connection so the rate limiter can share it.
func (c *Cache) Client() *redis.Client { return c.client }

func (c *Cache) Close() error { return c.client.Close() }

func (c *Cache) key(key string) string {
	if c.prefix == "" {
		return key
	}
	return c.prefix + ":" + key
}

func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := c.client.Get(ctx, c.key(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, domain.ErrMiss
	}
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return c.client.Set(ctx, c.key(key), value, ttl).Err()
}

func (c *Cache) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	prefixed := make([]string, 0, len(keys))
	for _, key := range keys {
		prefixed = append(prefixed, c.key(key))
	}
	return c.client.Del(ctx, prefixed...).Err()
}

// DeleteByPrefix drops a family of keys with SCAN, never KEYS.
//
// KEYS walks the entire keyspace in one blocking call; on a shared Redis under
// load that is a stall for every other client. SCAN pays a few round trips to
// avoid it.
func (c *Cache) DeleteByPrefix(ctx context.Context, prefix string) error {
	pattern := c.key(prefix) + "*"
	var cursor uint64
	for {
		keys, next, err := c.client.Scan(ctx, cursor, pattern, 256).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := c.client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}
