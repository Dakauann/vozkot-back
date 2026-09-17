package database

import (
	"fmt"

	"gorm.io/gorm"
)

// Splitting an order's total into the organiser's share and the box office's.
//
// Before the service fee existed, `orders.total_cents` was the face value and
// nothing else: the buyer paid exactly what the organiser priced the tier at.
// The new columns must say the same thing about those orders, which means
// subtotal = total and fee = 0.
//
// That is the ONE correct answer and it is worth being explicit about why: the
// tempting alternative is to derive the split from today's rate, and that would
// rewrite what somebody already paid. An order charged R$ 100 in August was not
// secretly R$ 90.91 of tickets and R$ 9.09 of commission; it was R$ 100 of
// tickets. A receipt reprinted next year has to keep saying so, and an
// organiser's payout report has to agree with it.
//
// Both statements are guarded so a re-run does nothing: `WHERE subtotal_cents
// = 0 AND total_cents <> 0` matches only rows that have never been backfilled,
// and a genuinely free order is already correct at zero.
func backfillOrderPricing(tx *gorm.DB) error {
	if !tx.Migrator().HasTable("orders") || !tx.Migrator().HasColumn("orders", "subtotal_cents") {
		// AutoMigrate has not added the column on this pass.
		return nil
	}

	if err := tx.Exec(`
		UPDATE orders
		SET subtotal_cents = total_cents, service_fee_cents = 0
		WHERE subtotal_cents = 0 AND total_cents <> 0`).Error; err != nil {
		return fmt.Errorf("backfill order pricing: %w", err)
	}

	if tx.Migrator().HasTable("order_items") && tx.Migrator().HasColumn("order_items", "fee_cents") {
		// The lines carry no fee either, for the same reason. total_cents on a
		// line was always the face value, so only the two new columns need
		// setting and they are already zero by default; the statement exists
		// for the case where a line was written by a mixed-version deploy with
		// a fee on the order and none on its lines, which would make the two
		// disagree.
		if err := tx.Exec(`
			UPDATE order_items i
			SET unit_fee_cents = 0, fee_cents = 0
			FROM orders o
			WHERE o.id = i.order_id
			  AND o.service_fee_cents = 0
			  AND (i.unit_fee_cents <> 0 OR i.fee_cents <> 0)`).Error; err != nil {
			return fmt.Errorf("backfill order item pricing: %w", err)
		}
	}

	// The refund policy an order was bought under. Orders written before
	// refunds existed carry 0, and usecases/refund reads that as the oldest
	// known policy rather than as today's — see the comment there. They are
	// deliberately NOT stamped with the current version: claiming an August
	// buyer agreed to rules written in September is exactly the lie freezing
	// exists to prevent.
	return nil
}
