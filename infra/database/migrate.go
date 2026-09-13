package database

import (
	"context"
	"fmt"
	"strings"

	"vozkot/infra/database/schema"

	"gorm.io/gorm"
)

const migrationLockID int64 = 847_560_221

// RunMigrations is idempotent and runs at API startup, matching Vozko's schema
// bootstrap convention. The transaction-scoped advisory lock makes concurrent
// replicas serialize their migration work.
func RunMigrations(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", migrationLockID).Error; err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		if err := dropLegacyTicketColumns(tx); err != nil {
			return err
		}
		// Type changes AutoMigrate cannot make run BEFORE it, or it fails on
		// the column it cannot cast.
		if err := rawResponseBytes(tx); err != nil {
			return err
		}
		// AutoMigrate keeps an index that exists by name, whatever its columns,
		// so one whose column order changed is dropped for it to come back right.
		if err := replaceClaimIndex(tx); err != nil {
			return err
		}
		if err := tx.AutoMigrate(
			&schema.User{},
			&schema.Session{},
			&schema.Ticket{},
			&schema.Media{},
			&schema.Order{},
			&schema.Job{},
			&schema.IdempotencyKey{},
		); err != nil {
			return fmt.Errorf("auto migrate: %w", err)
		}
		if err := createPartialIndexes(tx); err != nil {
			return err
		}
		if err := addStockGuards(tx); err != nil {
			return err
		}
		return nil
	})
}

// replaceClaimIndex drops the jobs claim index when an earlier revision built
// it with type as the leading column; the schema explains why the order
// matters. AutoMigrate recreates it in the same transaction.
func replaceClaimIndex(tx *gorm.DB) error {
	var definition string
	if err := tx.Raw(`SELECT indexdef FROM pg_indexes WHERE tablename = 'jobs' AND indexname = 'idx_jobs_claim'`).
		Scan(&definition).Error; err != nil {
		return fmt.Errorf("inspect claim index: %w", err)
	}
	if definition == "" || strings.Contains(definition, "(status, run_at, type)") {
		return nil
	}
	if err := tx.Exec(`DROP INDEX IF EXISTS idx_jobs_claim`).Error; err != nil {
		return fmt.Errorf("drop outdated claim index: %w", err)
	}
	return nil
}

// dropLegacyTicketColumns removes the columns of the helpdesk example the box
// office replaced.
//
// AutoMigrate adds and widens columns; it never drops one. Left in place, the
// old NOT NULL subject and requester_email would reject every insert the new
// schema makes, so the columns have to go before the new ones arrive.
func dropLegacyTicketColumns(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&schema.Ticket{}) {
		return nil
	}
	for _, column := range []string{"subject", "requester_email", "priority"} {
		if !tx.Migrator().HasColumn(&schema.Ticket{}, column) {
			continue
		}
		if err := tx.Migrator().DropColumn(&schema.Ticket{}, column); err != nil {
			return fmt.Errorf("drop legacy ticket column %q: %w", column, err)
		}
	}
	return nil
}

// createPartialIndexes adds the two indexes AutoMigrate cannot express.
//
// Both are partial, and both exist because an empty string is a value in SQL
// while the absence of one is not:
//
//   - jobs.dedupe_key must be unique only among OPEN jobs. A plain unique index
//     would let one completed "sync payment for order X" job block every future
//     sync for that order, forever.
//   - orders.payment_id must be unique only once a charge exists. Every order
//     starts without one, and a plain unique index would allow exactly one such
//     order per provider.
func createPartialIndexes(tx *gorm.DB) error {
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_dedupe_open
		 ON jobs (dedupe_key)
		 WHERE dedupe_key IS NOT NULL AND status IN ('pending', 'processing')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_provider_payment_present
		 ON orders (payment_provider, payment_id)
		 WHERE payment_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_orders_pending_reconcile
		 ON orders (updated_at, id)
		 WHERE status = 'pending_payment'`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_completed_retention
		 ON jobs (updated_at, id)
		 WHERE status = 'done'`,
		// The per-buyer hold cap runs on every checkout, so it has to stay cheap
		// for a buyer with years of history behind them. Partial on the only
		// status that holds stock, so the index carries open orders and nothing
		// else — a few rows per buyer, forever.
		`CREATE INDEX IF NOT EXISTS idx_orders_buyer_open_holds
		 ON orders (buyer_id, ticket_id)
		 WHERE status = 'pending_payment'`,
		// The settled-order audit pages oldest-checked-first through paid
		// orders, exactly as reconciliation does for pending ones.
		`CREATE INDEX IF NOT EXISTS idx_orders_paid_audit
		 ON orders (updated_at, id)
		 WHERE status = 'paid'`,
	}
	for _, statement := range statements {
		if err := tx.Exec(statement).Error; err != nil {
			return fmt.Errorf("create partial index: %w", err)
		}
	}
	// A database migrated by an earlier revision carries the total unique index
	// the struct tag used to declare; it is wrong (see the schema) and goes.
	if err := tx.Exec(`DROP INDEX IF EXISTS idx_orders_provider_payment`).Error; err != nil {
		return fmt.Errorf("drop total payment index: %w", err)
	}
	return nil
}

// rawResponseBytes converts an idempotency response column created as jsonb
// by an earlier revision into the raw bytes it must be. AutoMigrate cannot
// change a column's type across families on its own.
func rawResponseBytes(tx *gorm.DB) error {
	var dataType string
	err := tx.Raw(`
		SELECT data_type FROM information_schema.columns
		WHERE table_name = 'idempotency_keys' AND column_name = 'response'`).Scan(&dataType).Error
	if err != nil {
		return fmt.Errorf("inspect idempotency response column: %w", err)
	}
	if dataType != "jsonb" {
		return nil
	}
	if err := tx.Exec(`ALTER TABLE idempotency_keys
		ALTER COLUMN response TYPE bytea USING convert_to(response::text, 'UTF8')`).Error; err != nil {
		return fmt.Errorf("convert idempotency response column: %w", err)
	}
	return nil
}

// addStockGuards puts the inventory invariants in the database itself.
//
// The application is careful, and the application can have bugs. A CHECK
// constraint is the one guard that holds even when the code above it is wrong:
// no negative holds, no negative sales, and never more sold-plus-held than the
// tier was ever capable of.
//
// Each constraint is added only when absent. ADD CONSTRAINT takes an ACCESS
// EXCLUSIVE lock on the table, and a boot that re-ran it on every start would
// stall — or deadlock against — every checkout in flight during a rolling
// deploy. A steady-state boot must run no DDL at all.
func addStockGuards(tx *gorm.DB) error {
	guards := []struct {
		table, name, check string
	}{
		{"tickets", "chk_tickets_stock_bounds", "sold >= 0 AND reserved >= 0 AND sold + reserved <= quantity"},
		{"orders", "chk_orders_quantity_positive", "quantity > 0 AND total_cents >= 0"},
	}
	for _, guard := range guards {
		var present int64
		if err := tx.Raw(`SELECT COUNT(*) FROM pg_constraint WHERE conname = ?`, guard.name).Scan(&present).Error; err != nil {
			return fmt.Errorf("inspect constraint %s: %w", guard.name, err)
		}
		if present > 0 {
			continue
		}
		statement := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s)", guard.table, guard.name, guard.check)
		if err := tx.Exec(statement).Error; err != nil {
			return fmt.Errorf("apply stock guard %s: %w", guard.name, err)
		}
	}
	return nil
}
