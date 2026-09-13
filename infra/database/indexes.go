// Every index in this codebase is declared here, following Vozko's convention:
// one file, and a split by PURPOSE rather than by table.
//
//   - Schema constraints are RULES. A unique index that stops a job running
//     twice or an order being attached to a second payment is not tuning, and a
//     database without it accepts the duplicates the application believes are
//     impossible. A failure has to abort the boot.
//   - Performance indexes are SPEED. A missing one makes a query slow, never
//     wrong, and refusing to start over it turns a slow box office into a
//     closed one. A failure is logged, loudly, and the application runs.
//
// Where this departs from Vozko is the mechanism, not the organisation: every
// index here is built CONCURRENTLY and outside the migration transaction.
// Vozko builds its constraints inside the transaction with AutoMigrate, which
// is simpler and works until a replica boots while another is serving — and
// then it deadlocks, because a plain CREATE INDEX takes a lock on its table
// that every writer must wait for, while the transaction already holds a lock
// on a table those writers took first. This project hit exactly that: a booting
// replica's CREATE UNIQUE INDEX on `orders` held `jobs` from the AutoMigrate
// above it and waited on `orders`, while a checkout held `orders` and waited on
// `jobs` to write its outbox row. PostgreSQL broke the cycle by killing one,
// and the one it killed was somebody's purchase.
package database

import (
	"context"
	"fmt"
	"log"
	"sort"

	"gorm.io/gorm"
)

// indexLockID serialises the build. It is a SESSION lock, not a transaction
// one, because CREATE INDEX CONCURRENTLY cannot run inside a transaction.
const indexLockID int64 = 847_560_222

type indexDefinition struct {
	name      string
	statement string
}

// schemaConstraintIndexes are the rules. Both are partial, and both are partial
// because an empty string is a value in SQL while the absence of one is not.
var schemaConstraintIndexes = []indexDefinition{
	// A dedupe key is unique only among OPEN jobs. A total unique index would
	// let one completed "sync payment for order X" job block every future sync
	// for that order, forever.
	{"idx_jobs_dedupe_open", `
		CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_dedupe_open
		ON jobs (dedupe_key)
		WHERE dedupe_key IS NOT NULL AND status IN ('pending', 'processing')`},
	// A payment id is unique only once a charge exists. Every order starts
	// without one, and a total unique index would allow exactly one chargeless
	// order per provider — which is to say, one order.
	//
	// This is what makes "find the order this webhook is about" unambiguous, so
	// a database without it can attach one payment to two orders.
	{"idx_orders_provider_payment_present", `
		CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_provider_payment_present
		ON orders (payment_provider, payment_id)
		WHERE payment_id <> ''`},
}

// performanceIndexes are the sweeps' and the hot path's reading order. Each one
// is partial on the status its query filters by, so the index carries the rows
// that query looks at and nothing else — which is what keeps it small on a
// table that only grows.
var performanceIndexes = []indexDefinition{
	// Reconciliation pages oldest-checked-first through orders still waiting
	// for money.
	{"idx_orders_pending_reconcile", `
		CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_pending_reconcile
		ON orders (updated_at, id)
		WHERE status = 'pending_payment'`},
	// Retention deletes completed jobs oldest-first, in bounded batches.
	{"idx_jobs_completed_retention", `
		CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_completed_retention
		ON jobs (updated_at, id)
		WHERE status = 'done'`},
	// The per-buyer hold cap runs on EVERY checkout, so it has to stay cheap
	// for a buyer with years of history behind them. Partial on the only status
	// that holds stock, so this carries a few open orders per buyer and never
	// the rest of their life.
	//
	// The tier left this index when it left the orders table: the cap now
	// counts across order_items, so this one finds the buyer's open orders and
	// idx_order_items_order_ticket joins their lines.
	{"idx_orders_buyer_open_holds", `
		CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_buyer_open_holds
		ON orders (buyer_id)
		WHERE status = 'pending_payment'`},
	// The settled-order audit pages oldest-checked-first through paid orders,
	// exactly as reconciliation does for pending ones.
	{"idx_orders_paid_audit", `
		CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_orders_paid_audit
		ON orders (updated_at, id)
		WHERE status = 'paid'`},
}

// supersededIndexes were declared by an earlier revision of this schema and are
// wrong now. Dropped only when present, so a steady-state boot issues no DDL.
var supersededIndexes = []string{
	// The TOTAL unique index the struct tag used to declare, replaced by the
	// partial one above.
	"idx_orders_provider_payment",
	// Named a column that no longer exists. An index over (buyer_id,
	// ticket_id) cannot be created against a table without ticket_id, and the
	// replacement above carries the same name — so the old one is dropped
	// before the new one is built rather than left to fail forever.
	"idx_orders_ticket_id",
}

// createIndexes brings every index above into existence.
//
// It is a no-op on every boot after the first, and that is load-bearing rather
// than an optimisation: a steady-state boot must do no DDL and take no lock, or
// every replica in a rolling deploy queues behind the same one. So the
// catalogue is read first, and only a genuinely missing index costs anything.
func createIndexes(ctx context.Context, db *gorm.DB) error {
	all := append(append([]indexDefinition{}, schemaConstraintIndexes...), performanceIndexes...)

	missing, err := missingIndexes(ctx, db, all)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return dropSupersededIndexes(ctx, db)
	}

	// TRY the lock, never wait for it — and this is the subtle part.
	//
	// CREATE INDEX CONCURRENTLY waits for every transaction that could see the
	// table to finish. A session blocked on pg_advisory_lock is such a
	// transaction: it holds a virtual transaction id for as long as it waits.
	// So a second instance waiting politely for the lock is a transaction the
	// first instance's build must wait for, while the first holds the lock the
	// second is waiting on. PostgreSQL detects the cycle and kills one of them.
	// Waiting here does not merely cost time; it deadlocks the build it was
	// meant to protect.
	//
	// Failing to get the lock therefore means "another instance is already
	// building these", which is a reason to get out of their way rather than
	// queue behind them.
	var acquired bool
	if err := db.WithContext(ctx).Raw("SELECT pg_try_advisory_lock(?)", indexLockID).Scan(&acquired).Error; err != nil {
		return fmt.Errorf("acquire index lock: %w", err)
	}
	if !acquired {
		log.Printf("database: another instance is building %v; continuing without waiting", sortedNames(missing))
		return nil
	}
	defer func() {
		// Released on its own context: a build that ran out of the caller's
		// budget must still hand the lock back, or the next replica waits for
		// this connection to close.
		if err := db.WithContext(context.WithoutCancel(ctx)).
			Exec("SELECT pg_advisory_unlock(?)", indexLockID).Error; err != nil {
			log.Printf("database: release index lock: %v", err)
		}
	}()

	// Re-read under the lock: an instance that finished between the first check
	// and the lock has already done this work.
	missing, err = missingIndexes(ctx, db, all)
	if err != nil {
		return err
	}

	for _, index := range schemaConstraintIndexes {
		if !missing[index.name] {
			continue
		}
		if err := build(ctx, db, index); err != nil {
			// A rule the database is not enforcing is not something to warn
			// about and carry on from.
			return err
		}
	}
	for _, index := range performanceIndexes {
		if !missing[index.name] {
			continue
		}
		if err := build(ctx, db, index); err != nil {
			// Slow, not wrong. Refusing to boot here would close a box office
			// over a query plan.
			log.Printf("database: WARNING %v; queries it serves will be slower until it exists", err)
		}
	}

	return dropSupersededIndexes(ctx, db)
}

// build drops an unusable leftover and creates the index.
//
// An interrupted concurrent build leaves an INVALID index behind, which a later
// IF NOT EXISTS would happily skip forever: an index that exists, indexes
// nothing, and is never used. A unique one is worse than useless, because it is
// not enforcing the constraint the application believes it has. Dropping first
// is what makes an interrupted deploy self-healing on the next boot.
func build(ctx context.Context, db *gorm.DB, index indexDefinition) error {
	if err := db.WithContext(ctx).Exec(`DROP INDEX CONCURRENTLY IF EXISTS ` + index.name).Error; err != nil {
		return fmt.Errorf("drop invalid index %s: %w", index.name, err)
	}
	log.Printf("database: building index %s concurrently", index.name)
	if err := db.WithContext(ctx).Exec(index.statement).Error; err != nil {
		return fmt.Errorf("create index %s: %w", index.name, err)
	}
	return nil
}

// missingIndexes reports which of the given indexes are absent or invalid, in
// one catalogue query.
func missingIndexes(ctx context.Context, db *gorm.DB, indexes []indexDefinition) (map[string]bool, error) {
	names := make([]string, 0, len(indexes))
	for _, index := range indexes {
		names = append(names, index.name)
	}

	var present []string
	err := db.WithContext(ctx).Raw(`
		SELECT index_class.relname
		FROM pg_class index_class
		JOIN pg_index index_info ON index_info.indexrelid = index_class.oid
		WHERE index_class.relname IN ? AND index_info.indisvalid`, names).Scan(&present).Error
	if err != nil {
		return nil, fmt.Errorf("inspect indexes: %w", err)
	}

	valid := make(map[string]bool, len(present))
	for _, name := range present {
		valid[name] = true
	}
	missing := map[string]bool{}
	for _, name := range names {
		if !valid[name] {
			missing[name] = true
		}
	}
	return missing, nil
}

func dropSupersededIndexes(ctx context.Context, db *gorm.DB) error {
	for _, name := range supersededIndexes {
		var exists int64
		if err := db.WithContext(ctx).Raw(
			`SELECT COUNT(*) FROM pg_class WHERE relname = ?`, name).Scan(&exists).Error; err != nil {
			return fmt.Errorf("inspect superseded index %s: %w", name, err)
		}
		if exists == 0 {
			continue
		}
		log.Printf("database: dropping superseded index %s", name)
		if err := db.WithContext(ctx).Exec(`DROP INDEX CONCURRENTLY IF EXISTS ` + name).Error; err != nil {
			return fmt.Errorf("drop superseded index %s: %w", name, err)
		}
	}
	return nil
}

// sortedNames renders a set of index names for a log line, in a stable order.
func sortedNames(names map[string]bool) []string {
	listed := make([]string, 0, len(names))
	for name := range names {
		listed = append(listed, name)
	}
	sort.Strings(listed)
	return listed
}
