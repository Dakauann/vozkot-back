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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"

	"vozkot/infra/config"
	"vozkot/infra/crypto/pii"
	"vozkot/infra/crypto/piigorm"
	"vozkot/infra/crypto/vault"
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := ensureDatabase(ctx, cfg); err != nil {
			databaseErr = err
			return
		}

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
		Host:     envOr("TEST_DB_HOST", envOr("DB_HOST", "127.0.0.1")),
		Port:     envOr("TEST_DB_PORT", envOr("DB_PORT", "5433")),
		User:     envOr("TEST_DB_USER", envOr("DB_USER", "postgres")),
		Password: envOr("TEST_DB_PASSWORD", envOr("DB_PASSWORD", "postgres")),
		// A database OF ITS OWN, not the one the dev server is connected to.
		//
		// This used to fall back to DB_NAME, so `go test` wrote into the same
		// `vozkot` database a running server was polling. Everything about
		// that was wrong and all of it was visible in the dev log: the
		// server's queue picked up notification jobs the tests had raised,
		// then failed to decrypt their admission codes — the tests install
		// their own encryption keyring, so a code sealed under the test KEK
		// cannot be opened with the deployment's — and a test's cleanup
		// deleted job rows out from under a worker that had already claimed
		// them.
		//
		// The suffix is derived rather than fixed so a CI job that overrides
		// DB_NAME still gets a matching, separate database. testsupport runs
		// the migrations itself and creates the database on first use, so
		// nothing has to be provisioned by hand.
		Name:         envOr("TEST_DB_NAME", envOr("DB_NAME", "vozkot")+"_test"),
		SSLMode:      envOr("TEST_DB_SSLMODE", envOr("DB_SSLMODE", "disable")),
		MaxOpenConns: 20,
		MaxIdleConns: 10,
	}
	if cfg.Host == "" || cfg.Name == "" {
		return cfg, fmt.Errorf("database is not configured")
	}
	return cfg, nil
}

// ensureDatabase creates the test database if it is not there yet.
//
// Postgres cannot create a database from a connection to it, so this connects
// to the `postgres` maintenance database to ask. It is idempotent and silent:
// the common case is that the database already exists and this costs one
// connection at the start of a run.
func ensureDatabase(ctx context.Context, cfg config.DatabaseConfig) error {
	admin := cfg
	admin.Name = "postgres"
	admin.MaxOpenConns = 1
	admin.MaxIdleConns = 1

	handle, err := database.NewMigrationDatabase(ctx, admin)
	if err != nil {
		// Not fatal on its own: the database may already exist and the
		// maintenance connection may be the thing that is refused. Let the
		// real connection below report the truth.
		return nil
	}
	defer func() {
		if sqlDB, closeErr := handle.DB(); closeErr == nil {
			sqlDB.Close()
		}
	}()

	var exists bool
	if err := handle.WithContext(ctx).
		Raw("SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = ?)", cfg.Name).
		Row().Scan(&exists); err != nil {
		return nil
	}
	if exists {
		return nil
	}

	// The name cannot be a bind parameter in CREATE DATABASE, so it is quoted
	// as an identifier. It comes from this process's own environment rather
	// than from a request, and the quoting is belt and braces.
	quoted := `"` + strings.ReplaceAll(cfg.Name, `"`, `""`) + `"`
	if err := handle.WithContext(ctx).Exec("CREATE DATABASE " + quoted).Error; err != nil {
		return fmt.Errorf("create test database %s: %w", cfg.Name, err)
	}
	return nil
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

// encryptionOnce installs the keyring exactly once per test binary.
//
// piigorm holds the service as package state, set at startup, so installing it
// twice is harmless but pointless and installing it per test would race.
var encryptionOnce sync.Once

// Encryption installs a deterministic development keyring, so a test can
// exercise the columns that are sealed at rest.
//
// The keys are fixed bytes and that is fine here and nowhere else: they exist
// so a test can read back what it wrote, and every value they protect is
// synthetic. Production loads real keys through pii.LoadFromEnv, which refuses
// to start without them.
//
// Without this, any test touching a profile fails with "encryption service not
// configured" — which is the correct behaviour for the application and an
// unhelpful one for a suite that has to cover those columns.
func Encryption(t *testing.T) {
	t.Helper()
	encryptionOnce.Do(func() {
		keys := map[byte]*vault.Vault{}
		material := make([]byte, 32)
		for index := range material {
			material[index] = byte(index + 1)
		}
		built, err := vault.New(material, 1)
		if err != nil {
			t.Fatalf("testsupport: build development vault: %v", err)
		}
		keys[1] = built

		blind := make([]byte, 32)
		for index := range blind {
			blind[index] = byte(0xA0 + index)
		}
		service, err := pii.New(keys, 1, blind)
		if err != nil {
			t.Fatalf("testsupport: build encryption service: %v", err)
		}
		piigorm.SetService(service)
	})
}

// SeatedRoom is a materialised seat map: what a test needs to buy a chair.
type SeatedRoom struct {
	VenueID   string
	LayoutID  string
	SectionID string
	// LayoutSeatIDs are the definition seats, in row-then-seat order.
	LayoutSeatIDs []string
}

// SeedSeatedRoom builds a venue, a published layout and one seated section of
// rows x perRow chairs, and returns the ids.
//
// It seeds the DEFINITION only and does not materialise: binding a layout to an
// event is the thing under test in some of these, so the caller does it.
//
// It lives here rather than in a test file because two packages need it —
// checkout proves a chair cannot be claimed twice, payment proves a settled
// chair becomes an admission — and a second copy of a room would be a second
// place for the row letters and the seat ordering to drift.
func SeedSeatedRoom(t *testing.T, db *gorm.DB, ownerID string, rows, perRow int) SeatedRoom {
	t.Helper()
	room := SeatedRoom{
		VenueID:   Unique("ven"),
		LayoutID:  Unique("lay"),
		SectionID: Unique("sec"),
	}

	if err := db.Exec(`
		INSERT INTO venues (id, owner_id, name, capacity, created_at, updated_at)
		VALUES (?, ?, 'Teatro de Teste', ?, NOW(), NOW())`,
		room.VenueID, ownerID, rows*perRow).Error; err != nil {
		t.Fatalf("seed venue: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO venue_layouts
			(id, venue_id, owner_id, name, version, status, frozen,
			 view_box_width, view_box_height, created_at, updated_at)
		VALUES (?, ?, ?, 'Padrao', 1, 'published', false, 1000, 1000, NOW(), NOW())`,
		room.LayoutID, room.VenueID, ownerID).Error; err != nil {
		t.Fatalf("seed layout: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO layout_sections (id, layout_id, name, kind, capacity, display_order)
		VALUES (?, ?, 'Plateia A', 'seated', 0, 1)`,
		room.SectionID, room.LayoutID).Error; err != nil {
		t.Fatalf("seed section: %v", err)
	}

	// No row I, which is what most houses do and what the seat ordering
	// columns exist to survive: the labels are not sortable and the integers
	// are.
	const letters = "ABCDEFGHJKLMNPQR"
	for row := 0; row < rows; row++ {
		for seat := 1; seat <= perRow; seat++ {
			id := Unique("lst")
			err := db.Exec(`
				INSERT INTO layout_seats
					(id, section_id, row_label, seat_label, x, y, rotation, kind, row_order, seat_order)
				VALUES (?, ?, ?, ?, ?, ?, 0, 'standard', ?, ?)`,
				id, room.SectionID, string(letters[row%len(letters)]),
				strconv.Itoa(seat), float64(seat)*20, float64(row)*20, row, seat).Error
			if err != nil {
				t.Fatalf("seed layout seat: %v", err)
			}
			room.LayoutSeatIDs = append(room.LayoutSeatIDs, id)
		}
	}

	t.Cleanup(func() {
		db.Exec("DELETE FROM layout_seats WHERE section_id = ?", room.SectionID)
		db.Exec("DELETE FROM layout_sections WHERE layout_id = ?", room.LayoutID)
		db.Exec("DELETE FROM venue_layouts WHERE id = ?", room.LayoutID)
		db.Exec("DELETE FROM venues WHERE id = ?", room.VenueID)
	})
	return room
}

// CleanupEventSeats removes an event's materialised map when a test ends.
func CleanupEventSeats(t *testing.T, db *gorm.DB, eventID string) {
	t.Helper()
	t.Cleanup(func() {
		db.Exec("DELETE FROM event_seats WHERE event_id = ?", eventID)
		db.Exec("DELETE FROM event_seatings WHERE event_id = ?", eventID)
	})
}
