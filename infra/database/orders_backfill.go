package database

import (
	"fmt"

	"gorm.io/gorm"
)

// Moving an order's contents out of the order row and into line items.
//
// An order used to be one tier: `orders` carried ticket_id, quantity and
// unit_price_cents directly. It is now one EVENT covering one or more tiers,
// and what was bought lives in `order_items`.
//
// The migration has to be safe on a table that holds the record of money that
// changed hands, so it is written in the only order that is:
//
//  1. Create order_items.
//  2. Copy every existing order into exactly one line.
//  3. Derive each order's event from the tier that line names.
//  4. Only then drop the old columns.
//
// Dropping first and copying afterwards would put the money at the mercy of a
// crash in between. Copying first means a failure at any point leaves the old
// columns intact and the migration re-runnable, because every step below is
// written to be skipped when it has already been done.

// backfillOrderItems copies single-tier orders into their line item.
//
// Guarded by the old column still existing: once it is dropped there is nothing
// left to copy and nothing to do. The INSERT itself is guarded by NOT EXISTS
// rather than by a flag, so a run interrupted halfway resumes where it stopped
// instead of duplicating what it already wrote.
func backfillOrderItems(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("orders") || !tx.Migrator().HasColumn("orders", "ticket_id") {
		return nil
	}
	if !tx.Migrator().HasTable("order_items") {
		// AutoMigrate has not built it yet on this pass; the next statement
		// would fail on a table that does not exist.
		return nil
	}
	statement := `
		INSERT INTO order_items (id, order_id, ticket_id, ticket_title, quantity, unit_price_cents, total_cents, created_at)
		SELECT
			'oi_' || substr(md5(o.id), 1, 24),
			o.id,
			o.ticket_id,
			COALESCE(t.title, ''),
			o.quantity,
			o.unit_price_cents,
			o.total_cents,
			o.created_at
		FROM orders o
		LEFT JOIN tickets t ON t.id = o.ticket_id
		WHERE o.ticket_id <> ''
		  AND NOT EXISTS (SELECT 1 FROM order_items i WHERE i.order_id = o.id)`
	if err := tx.Exec(statement).Error; err != nil {
		return fmt.Errorf("backfill order items: %w", err)
	}
	return nil
}

// backfillOrderEvents derives each order's event from the tiers it covers.
//
// Through the items rather than through the old column, so it keeps working
// for orders written after the drop — and so an order whose tier has since been
// deleted is left alone rather than pointed at nothing.
func backfillOrderEvents(tx *gorm.DB) error {
	if !tx.Migrator().HasColumn("orders", "event_id") {
		return nil
	}
	statement := `
		UPDATE orders o
		SET event_id = t.event_id
		FROM order_items i
		JOIN tickets t ON t.id = i.ticket_id
		WHERE i.order_id = o.id
		  AND o.event_id = ''
		  AND t.event_id <> ''`
	if err := tx.Exec(statement).Error; err != nil {
		return fmt.Errorf("backfill order events: %w", err)
	}
	return nil
}

// dropSupersededOrderColumns removes what moved into order_items.
//
// Last, and only once every order has a line. Leaving them in place would be
// worse than untidy: the oversell audit sums `orders.quantity`, and a column
// that stops being written but keeps being read is how an audit starts passing
// for the wrong reason.
//
// The CHECK constraint over the old columns goes first, or the drop fails on a
// constraint that still names them.
func dropSupersededOrderColumns(tx *gorm.DB) error {
	if !tx.Migrator().HasColumn("orders", "ticket_id") {
		return nil
	}
	var orphans int64
	if err := tx.Raw(`
		SELECT COUNT(*) FROM orders o
		WHERE o.ticket_id <> ''
		  AND NOT EXISTS (SELECT 1 FROM order_items i WHERE i.order_id = o.id)`).
		Scan(&orphans).Error; err != nil {
		return fmt.Errorf("verify order item backfill: %w", err)
	}
	if orphans > 0 {
		// Refusing is the whole point. These columns are the only copy of what
		// those orders were for, and dropping them would destroy it.
		return fmt.Errorf("refusing to drop superseded order columns: %d order(s) have no line item", orphans)
	}

	if err := tx.Exec(`ALTER TABLE orders DROP CONSTRAINT IF EXISTS chk_orders_quantity_positive`).Error; err != nil {
		return fmt.Errorf("drop superseded order check: %w", err)
	}
	for _, column := range []string{"ticket_id", "quantity", "unit_price_cents"} {
		if !tx.Migrator().HasColumn("orders", column) {
			continue
		}
		if err := tx.Exec(fmt.Sprintf(`ALTER TABLE orders DROP COLUMN %s`, column)).Error; err != nil {
			return fmt.Errorf("drop superseded order column %q: %w", column, err)
		}
	}
	return nil
}

// relaxSupersededOrderColumns lets the old NOT NULL columns accept the rows
// AutoMigrate is about to write without them.
//
// It runs BEFORE the backfill, in the window where the new code is already
// inserting orders that name no tier while the old column still stands. Without
// it the first checkout after a deploy fails on a NOT NULL for a column the
// application no longer knows about.
func relaxSupersededOrderColumns(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("orders") {
		return nil
	}
	for _, column := range []string{"ticket_id", "quantity", "unit_price_cents"} {
		if !tx.Migrator().HasColumn("orders", column) {
			continue
		}
		statement := fmt.Sprintf(`ALTER TABLE orders ALTER COLUMN %s DROP NOT NULL`, column)
		if err := tx.Exec(statement).Error; err != nil {
			return fmt.Errorf("relax superseded order column %q: %w", column, err)
		}
	}
	return nil
}

// enforceOrderEventLink makes the event mandatory once every order has one.
//
// Applied only when nothing is left unlinked, so a database holding an order
// whose tier was deleted keeps running with a nullable column rather than
// failing to boot. That is the right trade: a missing event costs a listing its
// title, and refusing to start costs the box office everything.
func enforceOrderEventLink(tx *gorm.DB) error {
	if !tx.Migrator().HasColumn("orders", "event_id") {
		return nil
	}
	var unlinked int64
	if err := tx.Raw(`SELECT COUNT(*) FROM orders WHERE event_id IS NULL OR event_id = ''`).
		Scan(&unlinked).Error; err != nil {
		return fmt.Errorf("inspect unlinked orders: %w", err)
	}
	if unlinked > 0 {
		return nil
	}
	var present int64
	if err := tx.Raw(`SELECT COUNT(*) FROM pg_constraint WHERE conname = 'fk_orders_event'`).
		Scan(&present).Error; err != nil {
		return fmt.Errorf("inspect order event constraint: %w", err)
	}
	if present > 0 {
		return nil
	}
	statement := `ALTER TABLE orders
		ADD CONSTRAINT fk_orders_event FOREIGN KEY (event_id)
		REFERENCES events(id) ON UPDATE CASCADE ON DELETE RESTRICT`
	if err := tx.Exec(statement).Error; err != nil {
		return fmt.Errorf("link orders to events: %w", err)
	}
	return nil
}
