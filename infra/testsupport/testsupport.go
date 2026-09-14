// Package testsupport connects a test to the real infrastructure this system
// runs on: PostgreSQL, Redis and RabbitMQ.
//
// There are no in-memory stand-ins anywhere in this repository, and that is a
// deliberate constraint rather than an oversight. The behaviour these tests are
// actually checking IS the infrastructure's:
//
//   - Overselling is prevented by a conditional UPDATE and PostgreSQL's row
//     locks. A mutex in Go proves nothing about it.
//   - Exactly-once job execution comes from FOR UPDATE SKIP LOCKED.
//   - Idempotency comes from a unique index losing a race.
//   - The rate limiter is a Lua script's atomicity.
//
// A fake that reimplements those in Go can only ever test the fake.
//
// Every helper SKIPS when its service is unreachable, with the command that
// starts it, so a clone without Docker still runs the pure-domain suite.
package testsupport

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"

	"vozkot/infra/config"
	"vozkot/infra/database"
	redisCache "vozkot/infra/redis"
)

// Defaults point at this project's own compose services, which run on ports
// that do not collide with Vozko's.
const (
	defaultRedisURL    = "redis://127.0.0.1:6381/1"
	defaultRabbitMQURL = "amqp://guest:guest@127.0.0.1:5674/"
	startCommand       = "docker compose up -d database cache broker"
)

var (
	databaseOnce sync.Once
	sharedDB     *gorm.DB
	databaseErr  error
)

// Database returns a connection with the schema migrated, shared across the
// package's tests.
//
// Isolation comes from unique ids per test rather than from a fresh database:
// the suite then also exercises the indexes and constraints as they behave with
// data already in the tables.
func Database(t *testing.T) *gorm.DB {
	t.Helper()

	databaseOnce.Do(func() {
		cfg, err := databaseConfig()
		if err != nil {
			databaseErr = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		migrationDB, err := database.NewMigrationDatabase(ctx, cfg)
		if err != nil {
			databaseErr = err
			return
		}
		if err := database.RunMigrations(ctx, migrationDB); err != nil {
			databaseErr = err
			return
		}
		if sqlDB, err := migrationDB.DB(); err == nil {
			sqlDB.Close()
		}

		sharedDB, databaseErr = database.NewApplicationDatabase(ctx, cfg)
	})

	if databaseErr != nil {
		t.Skipf("PostgreSQL is not reachable (%v). Start it with: %s", databaseErr, startCommand)
	}
	return sharedDB
}

// DatabaseConfig is the test database's configuration, for a test that needs
// its own pool, one sized to reproduce exhaustion, say, rather than the
// shared connection.
func DatabaseConfig(t *testing.T) config.DatabaseConfig {
	t.Helper()
	cfg, err := databaseConfig()
	if err != nil {
		t.Skipf("PostgreSQL is not configured (%v). Start it with: %s", err, startCommand)
	}
	return cfg
}

func databaseConfig() (config.DatabaseConfig, error) {
	cfg := config.DatabaseConfig{
		Host:         envOr("TEST_DB_HOST", envOr("DB_HOST", "127.0.0.1")),
		Port:         envOr("TEST_DB_PORT", envOr("DB_PORT", "5433")),
		User:         envOr("TEST_DB_USER", envOr("DB_USER", "postgres")),
		Password:     envOr("TEST_DB_PASSWORD", envOr("DB_PASSWORD", "postgres")),
		Name:         envOr("TEST_DB_NAME", envOr("DB_NAME", "vozkot")),
		SSLMode:      envOr("TEST_DB_SSLMODE", envOr("DB_SSLMODE", "disable")),
		MaxOpenConns: 20,
		MaxIdleConns: 10,
	}
	if cfg.Host == "" || cfg.Name == "" {
		return cfg, fmt.Errorf("database is not configured")
	}
	return cfg, nil
}

// Cache returns a Redis connection on a test database index, flushed of this
// run's keys when the test ends.
func Cache(t *testing.T) *redisCache.Cache {
	t.Helper()

	url := envOr("TEST_REDIS_URL", defaultRedisURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connected, err := redisCache.Connect(ctx, url, "vozkot-test:"+Unique("run"))
	if err != nil {
		t.Skipf("Redis is not reachable at %s (%v). Start it with: %s", url, err, startCommand)
	}
	t.Cleanup(func() {
		// Only this run's prefix is removed, so a parallel run is untouched.
		_ = connected.DeleteByPrefix(context.Background(), "")
		_ = connected.Close()
	})
	return connected
}

// RabbitMQURL returns the broker URL, skipping when nothing answers on it.
func RabbitMQURL(t *testing.T) string {
	t.Helper()

	url := envOr("TEST_RABBITMQ_URL", defaultRabbitMQURL)
	connection, err := amqp.DialConfig(url, amqp.Config{Heartbeat: 5 * time.Second, Locale: "en_US"})
	if err != nil {
		t.Skipf("RabbitMQ is not reachable at %s (%v). Start it with: %s", url, err, startCommand)
	}
	_ = connection.Close()
	return url
}

var counter struct {
	sync.Mutex
	value int
}

// Unique builds an identifier no other test will use, so tests share a database
// without sharing rows.
func Unique(prefix string) string {
	counter.Lock()
	counter.value++
	sequence := counter.value
	counter.Unlock()
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), sequence)
}

// SeedEvent creates the event a ticket tier has to belong to, and returns its
// id.
//
// Raw SQL rather than the repository, so this stays usable from the repository
// package's own tests without importing it. Published rather than draft: a test
// that seeds a tier almost always goes on to sell from it, and a draft event
// would be invisible to every listing that looks.
func SeedEvent(t *testing.T, db *gorm.DB, ownerID string) string {
	t.Helper()
	id := Unique("evt")
	err := db.Exec(`
		INSERT INTO events
			(id, owner_id, slug, name, description, category, venue, address,
			 neighborhood, city, uf, postal_code, starts_at, status, created_at, updated_at)
		VALUES (?, ?, ?, 'Festival Aurora', '', 'festas_shows', 'Arena Castelao', '',
		        '', 'Fortaleza', 'CE', '', ?, 'published', NOW(), NOW())`,
		id, ownerID, id, time.Now().Add(720*time.Hour).UTC()).Error
	if err != nil {
		t.Fatalf("seed event: %v", err)
	}
	t.Cleanup(func() {
		// Tiers reference the event with ON DELETE RESTRICT, so they go first.
		db.Exec("DELETE FROM tickets WHERE event_id = ?", id)
		db.Exec("DELETE FROM events WHERE id = ?", id)
	})
	return id
}

// CountJobs counts jobs of one type in one status. Tests give their jobs a
// unique type, so counting by type is what keeps packages that run in parallel
// against the same database from seeing each other's rows: the alternative,
// truncating shared tables, wipes another package's test out from under it.
func CountJobs(t *testing.T, db *gorm.DB, jobType, status string) int64 {
	t.Helper()
	var total int64
	if err := db.Raw("SELECT COUNT(*) FROM jobs WHERE type = ? AND status = ?", jobType, status).Scan(&total).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return total
}

// CleanupJobs removes a test's own jobs when it ends.
func CleanupJobs(t *testing.T, db *gorm.DB, jobTypes ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, jobType := range jobTypes {
			db.Exec("DELETE FROM jobs WHERE type = ?", jobType)
		}
	})
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
