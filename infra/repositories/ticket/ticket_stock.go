package ticket

import (
	"context"
	"fmt"
	"time"

	domain "vozkot/domain/ticket"
	"vozkot/infra/database/schema"

	"gorm.io/gorm"
)

// Inventory movements.
//
// Every one of these is a SINGLE conditional statement, and that is the whole
// point. Reading a ticket, deciding in Go that three are left, and writing back
// is the textbook oversell: two requests read the same three and both take two.
// Expressing the condition inside the UPDATE makes the database the arbiter, and
// the database resolves it by locking the row, so of two concurrent buyers for
// the last ticket, exactly one gets a row back and the other gets none.

// Reserve holds `quantity` tickets, reporting false when there were not enough.
func (r *TicketRepository) Reserve(ctx context.Context, ticketID string, quantity int) (bool, error) {
	if quantity <= 0 {
		return false, domain.ErrInvalidQuantity
	}
	result := r.db.WithContext(ctx).Model(&schema.Ticket{}).
		Where("id = ? AND status = ? AND quantity - sold - reserved >= ?",
			ticketID, string(domain.StatusOnSale), quantity).
		// A tier bound to a standing floor or a box is also capped by the room
		// itself, so raising the tier's quantity can never sell an eleventh
		// place in a ten-person box. MIN, not LIMIT 1: if the one-tier-per-area
		// rule were ever broken by hand, the smallest capacity is the safe read.
		// Unbound tiers fall back to `quantity`, which is the column, so this
		// restates the guard above and changes nothing for them.
		Where(`sold + reserved + ? <= COALESCE((
			SELECT MIN((area.value->>'capacity')::int)
			FROM event_seatings manifest,
			     LATERAL jsonb_each(COALESCE(manifest.areas, '{}'::jsonb)) area
			WHERE manifest.event_id = tickets.event_id
			  AND area.value->>'ticketId' = tickets.id
		), quantity)`, quantity).
		Updates(map[string]any{
			"reserved":   gorm.Expr("reserved + ?", quantity),
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return false, result.Error
	}
	// No row matched: sold out, held out, or no longer on sale. The caller
	// cannot tell which, and does not need to; all three mean "not yours".
	return result.RowsAffected == 1, nil
}

// Release returns held stock to availability.
func (r *TicketRepository) Release(ctx context.Context, ticketID string, quantity int) error {
	if quantity <= 0 {
		return domain.ErrInvalidQuantity
	}
	result := r.db.WithContext(ctx).Model(&schema.Ticket{}).
		Where("id = ? AND reserved >= ?", ticketID, quantity).
		Updates(map[string]any{
			"reserved":   gorm.Expr("reserved - ?", quantity),
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		// The guard refuses to drive `reserved` negative. Reaching it means the
		// same hold was released twice, which is a bug worth surfacing rather
		// than a number worth clamping.
		return fmt.Errorf("release %d ticket(s) of %s: no matching hold", quantity, ticketID)
	}
	return nil
}

// Commit turns a hold into a sale. It is the only path to `sold`.
func (r *TicketRepository) Commit(ctx context.Context, ticketID string, quantity int) error {
	if quantity <= 0 {
		return domain.ErrInvalidQuantity
	}
	result := r.db.WithContext(ctx).Model(&schema.Ticket{}).
		Where("id = ? AND reserved >= ?", ticketID, quantity).
		Updates(map[string]any{
			"reserved":   gorm.Expr("reserved - ?", quantity),
			"sold":       gorm.Expr("sold + ?", quantity),
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("commit %d ticket(s) of %s: no matching hold", quantity, ticketID)
	}
	return nil
}

// ReleaseSold gives a paid ticket back, for a refund or a chargeback.
func (r *TicketRepository) ReleaseSold(ctx context.Context, ticketID string, quantity int) error {
	if quantity <= 0 {
		return domain.ErrInvalidQuantity
	}
	result := r.db.WithContext(ctx).Model(&schema.Ticket{}).
		Where("id = ? AND sold >= ?", ticketID, quantity).
		Updates(map[string]any{
			"sold":       gorm.Expr("sold - ?", quantity),
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("release %d sold ticket(s) of %s: no matching sale", quantity, ticketID)
	}
	return nil
}
