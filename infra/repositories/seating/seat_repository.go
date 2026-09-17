// Package seating persists reserved seating: the layout an organiser draws and
// the named seats a buyer claims from it.
package seating

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	domain "vozkot/domain/seating"
	"vozkot/infra/database/schema"
)

// SeatRepository is the inventory side: claim, release, commit, read.
type SeatRepository struct {
	db *gorm.DB
}

func NewSeatRepository(db *gorm.DB) *SeatRepository { return &SeatRepository{db: db} }

var _ domain.Repository = (*SeatRepository)(nil)

// nextVersion is the sequence expression every status change stamps itself
// with, so a map poll can ask "what changed since 417".
//
// A literal in the SQL rather than a parameter: it is an identifier, it comes
// from a constant in this codebase and never from a request, and nextval()
// cannot be parameterised anyway.
const nextVersion = "nextval('" + schema.SeatVersionSequence + "')"

// Claim holds the named seats for an order, all or nothing.
//
// ONE conditional UPDATE. That is the entire concurrency design, and it is the
// same design tier stock already uses — the row lock PostgreSQL takes to
// evaluate `status = 'available'` IS the mutual exclusion, so two buyers
// asking for FILA K POLTRONA 12 in the same instant produce exactly one
// holder and one loser, with no lock held in application code.
//
// Three things in the WHERE clause are load-bearing:
//
//   - `status = 'available'` is the race. Everything else is bookkeeping.
//   - `ticket_id = ?` closes a price hole. Without it a buyer could send a
//     premium seat's id on a cheap tier's line and pay the cheap price for the
//     good chair.
//   - `event_id = ?` closes the same hole across nights.
//
// It does NOT roll anything back on a partial claim, and it does not return an
// error for one. Losing a seat during an onsale is the ordinary outcome, not a
// fault; the caller is inside a transaction and returning the result upward is
// what lets the use case decide. What the caller gets is which seats went, by
// name, because "one of your four seats is gone" is not something a buyer can
// act on and "K12 went, the other three are still yours" is.
func (r *SeatRepository) Claim(ctx context.Context, request domain.ClaimRequest) (domain.ClaimResult, error) {
	if err := request.Validate(); err != nil {
		return domain.ClaimResult{}, err
	}
	now := time.Now().UTC()

	var claimed []schema.EventSeat
	err := r.db.WithContext(ctx).Raw(`
		UPDATE event_seats
		   SET status = ?, order_id = ?, hold_expires_at = ?,
		       version = `+nextVersion+`, updated_at = ?
		 WHERE event_id = ? AND ticket_id = ? AND status = ? AND id IN ?
		RETURNING *`,
		string(domain.StatusHeld), request.OrderID, request.HoldExpiresAt.UTC(), now,
		request.EventID, request.TicketID, string(domain.StatusAvailable), request.SeatIDs,
	).Scan(&claimed).Error
	if err != nil {
		return domain.ClaimResult{}, err
	}

	result := domain.ClaimResult{Claimed: toDomainSeats(claimed)}
	if len(claimed) == len(request.SeatIDs) {
		return result, nil
	}

	// Something was taken from under this request. Read back exactly what, in
	// the same transaction, so the answer describes the state the claim lost
	// to rather than a guess assembled from ids.
	taken := make(map[string]struct{}, len(claimed))
	for index := range claimed {
		taken[claimed[index].ID] = struct{}{}
	}
	missing := make([]string, 0, len(request.SeatIDs)-len(claimed))
	for _, id := range request.SeatIDs {
		if _, ok := taken[id]; !ok {
			missing = append(missing, id)
		}
	}

	var unavailable []schema.EventSeat
	if err := r.db.WithContext(ctx).
		Where("event_id = ? AND id IN ?", request.EventID, missing).
		Find(&unavailable).Error; err != nil {
		return domain.ClaimResult{}, err
	}
	result.Unavailable = toDomainSeats(unavailable)

	// A seat id that names no row at all is not "unavailable", it is wrong, and
	// silently reporting it as taken would send a buyer back to a picker that
	// shows it free. Synthesise it as a not-found seat so the caller can tell
	// the two apart by the empty label.
	if len(result.Unavailable) < len(missing) {
		found := make(map[string]struct{}, len(unavailable))
		for index := range unavailable {
			found[unavailable[index].ID] = struct{}{}
		}
		for _, id := range missing {
			if _, ok := found[id]; !ok {
				result.Unavailable = append(result.Unavailable, domain.EventSeat{
					ID:      id,
					EventID: request.EventID,
				})
			}
		}
	}
	return result, nil
}

// ReleaseForOrder returns an order's HELD seats to availability.
//
// Scoped to held, which is the guard that matters: a lapsed-hold sweep arriving
// after settlement must not put a paid seat back on sale. The tier's own
// Release has the same shape and the same reason.
func (r *SeatRepository) ReleaseForOrder(ctx context.Context, orderID string) (int, error) {
	result := r.db.WithContext(ctx).Exec(`
		UPDATE event_seats
		   SET status = ?, order_id = NULL, hold_expires_at = NULL,
		       version = `+nextVersion+`, updated_at = ?
		 WHERE order_id = ? AND status = ?`,
		string(domain.StatusAvailable), time.Now().UTC(),
		orderID, string(domain.StatusHeld))
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

// CommitForOrder turns an order's held seats into sold.
//
// Runs in the transaction that marks the order paid, beside the tier's Commit.
// Scoped to held, so a redelivered settlement moves nothing and says so by
// returning zero — which is how a caller tells a first settlement from a
// repeat without asking a second question.
func (r *SeatRepository) CommitForOrder(ctx context.Context, orderID string) (int, error) {
	result := r.db.WithContext(ctx).Exec(`
		UPDATE event_seats
		   SET status = ?, hold_expires_at = NULL,
		       version = `+nextVersion+`, updated_at = ?
		 WHERE order_id = ? AND status = ?`,
		string(domain.StatusSold), time.Now().UTC(),
		orderID, string(domain.StatusHeld))
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

// ReleaseSoldForOrder puts an order's sold seats back on sale, for a refund.
//
// Scoped to sold, which makes it the exact mirror of ReleaseForOrder: between
// them, a seat can only ever leave an order through the door that matches how
// it arrived. A single "release whatever this order has" would let the
// lapsed-hold sweep free a paid chair.
func (r *SeatRepository) ReleaseSoldForOrder(ctx context.Context, orderID string) (int, error) {
	result := r.db.WithContext(ctx).Exec(`
		UPDATE event_seats
		   SET status = ?, order_id = NULL, hold_expires_at = NULL,
		       version = `+nextVersion+`, updated_at = ?
		 WHERE order_id = ? AND status = ?`,
		string(domain.StatusAvailable), time.Now().UTC(),
		orderID, string(domain.StatusSold))
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

// ListByOrder is what a receipt, a wallet and the door read.
func (r *SeatRepository) ListByOrder(ctx context.Context, orderID string) ([]domain.EventSeat, error) {
	var records []schema.EventSeat
	if err := r.db.WithContext(ctx).
		Where("order_id = ?", orderID).
		Order("section_name, row_order, seat_order").
		Find(&records).Error; err != nil {
		return nil, err
	}
	return toDomainSeats(records), nil
}

// ListByEvent is the map.
//
// sinceVersion of 0 returns every seat; anything higher returns only what has
// changed, which is what makes polling a busy onsale cost an indexed range
// scan instead of the whole house.
func (r *SeatRepository) ListByEvent(ctx context.Context, eventID string, sinceVersion int64) ([]domain.EventSeat, error) {
	query := r.db.WithContext(ctx).Where("event_id = ?", eventID)
	if sinceVersion > 0 {
		query = query.Where("version > ?", sinceVersion)
	}
	var records []schema.EventSeat
	if err := query.Order("section_name, row_order, seat_order").Find(&records).Error; err != nil {
		return nil, err
	}
	return toDomainSeats(records), nil
}

// CountsByEvent tallies each tier's seats by status.
//
// This is what proves the tier counters have not drifted from the seats they
// project. One grouped scan of a partial index, not a row-by-row walk.
func (r *SeatRepository) CountsByEvent(ctx context.Context, eventID string) ([]domain.Counts, error) {
	var rows []struct {
		TicketID  string
		Available int
		Held      int
		Sold      int
		Blocked   int
	}
	if err := r.db.WithContext(ctx).Raw(`
		SELECT ticket_id,
		       COUNT(*) FILTER (WHERE status = ?) AS available,
		       COUNT(*) FILTER (WHERE status = ?) AS held,
		       COUNT(*) FILTER (WHERE status = ?) AS sold,
		       COUNT(*) FILTER (WHERE status = ?) AS blocked
		  FROM event_seats
		 WHERE event_id = ?
		 GROUP BY ticket_id`,
		string(domain.StatusAvailable), string(domain.StatusHeld),
		string(domain.StatusSold), string(domain.StatusBlocked),
		eventID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	counts := make([]domain.Counts, 0, len(rows))
	for _, row := range rows {
		counts = append(counts, domain.Counts{
			TicketID:  row.TicketID,
			Available: row.Available,
			Held:      row.Held,
			Sold:      row.Sold,
			Blocked:   row.Blocked,
		})
	}
	return counts, nil
}

// Block withholds seats from sale.
//
// Scoped to available: blocking must never cancel a ticket somebody holds. An
// organiser marking a broken chair on the night gets a count back that is
// lower than what they asked for, which is the honest answer and lets the UI
// say "3 of 4 blocked; one is sold".
func (r *SeatRepository) Block(ctx context.Context, eventID string, seatIDs []string, reason domain.BlockReason) (int, error) {
	seatIDs = domain.NormalizeSeatIDs(seatIDs)
	if len(seatIDs) == 0 {
		return 0, nil
	}
	result := r.db.WithContext(ctx).Exec(`
		UPDATE event_seats
		   SET status = ?, block_reason = ?, version = `+nextVersion+`, updated_at = ?
		 WHERE event_id = ? AND status = ? AND id IN ?`,
		string(domain.StatusBlocked), string(reason), time.Now().UTC(),
		eventID, string(domain.StatusAvailable), seatIDs)
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

// Unblock puts blocked seats back on sale.
func (r *SeatRepository) Unblock(ctx context.Context, eventID string, seatIDs []string) (int, error) {
	seatIDs = domain.NormalizeSeatIDs(seatIDs)
	if len(seatIDs) == 0 {
		return 0, nil
	}
	result := r.db.WithContext(ctx).Exec(`
		UPDATE event_seats
		   SET status = ?, block_reason = '', version = `+nextVersion+`, updated_at = ?
		 WHERE event_id = ? AND status = ? AND id IN ?`,
		string(domain.StatusAvailable), time.Now().UTC(),
		eventID, string(domain.StatusBlocked), seatIDs)
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

// SeatingOf reports the manifest an event is selling, if any.
func (r *SeatRepository) SeatingOf(ctx context.Context, eventID string) (*domain.EventSeating, error) {
	var record schema.EventSeating
	err := r.db.WithContext(ctx).Clauses(clause.Locking{Strength: "SHARE"}).Where("event_id = ?", eventID).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var areas map[string]domain.AreaBinding
	if len(record.Areas) > 0 {
		if err := json.Unmarshal(record.Areas, &areas); err != nil {
			return nil, err
		}
	}
	return &domain.EventSeating{
		Areas:          areas,
		EventID:        record.EventID,
		LayoutID:       record.LayoutID,
		LayoutVersion:  record.LayoutVersion,
		SeatCount:      record.SeatCount,
		BlockedCount:   record.BlockedCount,
		MaterialisedAt: record.MaterialisedAt,
	}, nil
}

func toDomainSeats(records []schema.EventSeat) []domain.EventSeat {
	seats := make([]domain.EventSeat, 0, len(records))
	for index := range records {
		seats = append(seats, toDomainSeat(&records[index]))
	}
	return seats
}

func toDomainSeat(record *schema.EventSeat) domain.EventSeat {
	seat := domain.EventSeat{
		ID:       record.ID,
		EventID:  record.EventID,
		TicketID: record.TicketID,
		Label: domain.Label{
			Section: record.SectionName,
			Row:     record.RowLabel,
			Seat:    record.SeatLabel,
		},
		Kind:          domain.SeatKind(record.Kind),
		Status:        domain.Status(record.Status),
		HoldExpiresAt: record.HoldExpiresAt,
		BlockReason:   domain.BlockReason(record.BlockReason),
		Version:       record.Version,
		X:             record.X,
		Y:             record.Y,
		RowOrder:      record.RowOrder,
		SeatOrder:     record.SeatOrder,
		UpdatedAt:     record.UpdatedAt,
	}
	if record.LayoutSeatID != nil {
		seat.LayoutSeatID = *record.LayoutSeatID
	}
	if record.OrderID != nil {
		seat.OrderID = *record.OrderID
	}
	return seat
}
