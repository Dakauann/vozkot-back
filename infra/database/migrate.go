package database

import (
	"context"
	"fmt"
	"log"
	"strings"

	"vozkot/infra/database/schema"

	"gorm.io/gorm"
)

const migrationLockID int64 = 847_560_221

// RunMigrations is idempotent and runs at API startup, matching Vozko's schema
// bootstrap convention. The transaction-scoped advisory lock makes concurrent
// replicas serialize their migration work.
//
// It is schema only. Every index this codebase has is declared in indexes.go
// and built after this transaction commits, which is a departure from Vozko
// worth knowing about before anyone moves them back in: an index built inside
// this transaction blocks writers to its table for the whole build, and this
// transaction already holds locks those writers took in the other order. See
// indexes.go for the deadlock that is, and how it showed up.
func RunMigrations(ctx context.Context, db *gorm.DB) error {
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
		// The columns that moved up to the event stop being required on a tier
		// BEFORE AutoMigrate touches the table, or it fails on the NOT NULL it
		// can no longer satisfy.
		if err := relaxSupersededTicketColumns(tx); err != nil {
			return err
		}
		// Two passes, and the order is the whole point.
		//
		// Events must exist before the backfill can write any, and tickets
		// cannot be migrated against a `not null` event_id until every tier
		// HAS one. So: create events, add the column nullable, backfill, apply
		// the constraint — and only then let AutoMigrate compare the tickets
		// struct against a table that already matches it. Migrating tickets
		// first would leave the column nullable in the database and `not null`
		// in the tag, and AutoMigrate would try to reconcile the two on every
		// boot from then on.
		//
		// The password column stops being required in the same pass, and for
		// the same class of reason: sign-in is a code sent to an address, so
		// the new code inserts users with no password while the old NOT NULL
		// still stands.
		if err := relaxPasswordColumn(tx); err != nil {
			return err
		}
		if err := tx.AutoMigrate(
			&schema.User{},
			&schema.VerificationChallenge{},
			&schema.Session{},
			&schema.Event{},
		); err != nil {
			return fmt.Errorf("auto migrate events: %w", err)
		}
		if err := addEventIDColumn(tx); err != nil {
			return err
		}
		if err := backfillEvents(tx); err != nil {
			return err
		}
		if err := enforceEventLink(tx); err != nil {
			return err
		}
		// Artwork follows the event, and has to be repointed while the tier it
		// used to name is still resolvable.
		if err := moveMediaToEvents(tx); err != nil {
			return err
		}
		// The order columns that moved into line items stop being required
		// BEFORE AutoMigrate adds the tables that supersede them, for the same
		// reason the tier columns do: in the window between the deploy and this
		// migration, the new code is already writing orders that name no tier.
		if err := relaxSupersededOrderColumns(tx); err != nil {
			return err
		}
		if err := tx.AutoMigrate(
			&schema.Ticket{},
			&schema.Media{},
			&schema.Order{},
			&schema.OrderItem{},
			&schema.Job{},
			&schema.IdempotencyKey{},
		); err != nil {
			return fmt.Errorf("auto migrate: %w", err)
		}
		// Copy, derive, verify, drop — in that order, and never the other way
		// round. See orders_backfill.go for why each step is guarded the way it
		// is.
		if err := backfillOrderItems(tx); err != nil {
			return err
		}
		if err := backfillOrderEvents(tx); err != nil {
			return err
		}
		if err := dropSupersededOrderColumns(tx); err != nil {
			return err
		}
		if err := enforceOrderEventLink(tx); err != nil {
			return err
		}
		if err := addSearchVector(tx); err != nil {
			return err
		}
		if err := addStockGuards(tx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Outside the transaction, and before the indexes: a trigram index cannot
	// be built until the extension exists.
	enableTrigramSearch(ctx, db)

	// Indexes live in indexes.go and are built outside this transaction, on
	// purpose. See that file for why.
	return createIndexes(ctx, db)
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

// addSearchVector gives events a real full-text index.
//
// The obvious implementation of a catalogue search is `name ILIKE '%termo%'`,
// and it is wrong in two ways that both get worse with success: no index can
// serve a leading wildcard, so every search scans the whole table; and it
// matches substrings rather than words, so "arte" matches "Bartender" and a
// search for "shows" finds nothing called "show".
//
// PostgreSQL's own full-text search fixes both. The vector is a GENERATED
// column, so it is maintained by the database on every write and cannot drift
// from the row the way a trigger-updated or application-updated column does.
// The weights are what make the ranking useful: a term in the name (A) should
// beat the same term buried in a description (C), or searching for a city
// returns every event that merely mentions it.
//
// The 'portuguese' configuration is the point of using this at all — it stems,
// so "shows" finds "show" and "festas" finds "festa", and it drops stop words
// so "a casa" searches for "casa".
func addSearchVector(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("events") {
		return nil
	}
	if tx.Migrator().HasColumn("events", "search_vector") {
		return nil
	}
	statement := `
		ALTER TABLE events ADD COLUMN search_vector tsvector
		GENERATED ALWAYS AS (
			setweight(to_tsvector('portuguese', coalesce(name, '')), 'A') ||
			setweight(to_tsvector('portuguese', coalesce(venue, '')), 'B') ||
			setweight(to_tsvector('portuguese', coalesce(city, '')), 'B') ||
			setweight(to_tsvector('portuguese', coalesce(description, '')), 'C')
		) STORED`
	if err := tx.Exec(statement).Error; err != nil {
		return fmt.Errorf("add event search vector: %w", err)
	}
	return nil
}

// enableTrigramSearch turns on the extension that makes a misspelling findable.
//
// Full-text search matches WORDS, so a buyer who types "festivl" gets nothing
// at all — and a search box that punishes a typo with an empty page is one
// people stop using. Trigram similarity catches those, and the two are used
// together: full text decides relevance, trigram decides that "festivl" meant
// "festival".
//
// CREATE EXTENSION needs privileges a locked-down production role may not have,
// so a failure is logged and the application runs. The search then still works,
// exactly and without typo tolerance, which is a degradation rather than an
// outage.
func enableTrigramSearch(ctx context.Context, db *gorm.DB) {
	if err := db.WithContext(ctx).Exec(`CREATE EXTENSION IF NOT EXISTS pg_trgm`).Error; err != nil {
		log.Printf("database: WARNING pg_trgm is unavailable (%v); search will not tolerate typos", err)
	}
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
		{"orders", "chk_orders_total_not_negative", "total_cents >= 0"},
		// The quantity guard moved down to the line, which is where a quantity
		// now lives. An order's total is the sum of its items; a line of zero
		// tickets is a hold on nothing and must never reach the table.
		{"order_items", "chk_order_items_quantity_positive", "quantity > 0 AND unit_price_cents >= 0 AND total_cents >= 0"},
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
