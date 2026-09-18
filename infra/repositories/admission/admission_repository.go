// Package admission persists credentials and spends them exactly once.
//
// The two methods that matter here are IssueForOrder and Admit, and both are
// about a guarantee the database makes and Go cannot:
//
//   - a code is unique because a UNIQUE index says so, not because the
//     generator is good;
//   - an admission is spent once because a conditional UPDATE says so, not
//     because the use case read it first.
package admission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	domain "vozkot/domain/admission"
	seatingdomain "vozkot/domain/seating"
	"vozkot/infra/crypto/piigorm"
	"vozkot/infra/database/schema"
)

type AdmissionRepository struct {
	db *gorm.DB
}

func NewAdmissionRepository(db *gorm.DB) *AdmissionRepository {
	return &AdmissionRepository{db: db}
}

var _ domain.Repository = (*AdmissionRepository)(nil)

// codeCollisionAttempts is how many times a draw may lose the unique-index
// race before the issue fails.
//
// Three is generous to the point of being theatrical: at 54 bits the second
// attempt is already a coincidence nobody will see, and the retry exists to
// turn a cosmic-ray-grade event into a redraw instead of a failed sale. More
// than a handful of attempts would mean the index is being violated for some
// other reason, and failing loudly is then the right answer.
const codeCollisionAttempts = 3

// IssueForOrder mints the admissions a paid order is owed.
//
// Idempotent by the unique index on (order_id, ticket_id, sequence) rather
// than by asking first. Checking for existing rows and then inserting would
// leave a window two settlements of the same order can both pass through, and
// a redelivered webhook is exactly two settlements of the same order. Here the
// second one collides and is told it created nothing.
func (r *AdmissionRepository) IssueForOrder(
	ctx context.Context,
	order domain.OrderLines,
) ([]domain.Admission, error) {
	if order.Total() == 0 {
		return nil, nil
	}

	// Already issued? Answered first because it is the common case for a
	// redelivery and costs one indexed read, and because it keeps the happy
	// path from generating codes it is about to throw away.
	var existing int64
	if err := r.db.WithContext(ctx).Model(&schema.Admission{}).
		Where("order_id = ?", order.OrderID).Count(&existing).Error; err != nil {
		return nil, err
	}
	if existing > 0 {
		return nil, nil
	}

	issued := make([]domain.Admission, 0, order.Total())
	records := make([]schema.Admission, 0, order.Total())
	now := time.Now().UTC()

	// The sequence runs per ORDER AND TIER, continuously across lines, not from
	// 1 inside each one.
	//
	// It used to restart per line and that was correct while a line was a
	// counted quantity: one line per tier meant the two were the same thing.
	// A seated line is one CHAIR, so four seats of Plateia Premium are four
	// lines of the same tier, and restarting would mint four admissions all
	// numbered 1, which collides on the unique index that makes issuing
	// idempotent, and aborts the settlement transaction that was taking the
	// money.
	//
	// Running it continuously also keeps "2 de 3" printable on a seated ticket,
	// which is the reason the number exists at all.
	sequences := make(map[string]int, len(order.Lines))
	for _, line := range order.Lines {
		for issuedOnLine := 1; issuedOnLine <= line.Quantity; issuedOnLine++ {
			sequences[line.TicketID]++
			sequence := sequences[line.TicketID]
			item, err := domain.New(newID(), domain.Draft{
				OrderID:     order.OrderID,
				EventID:     order.EventID,
				TicketID:    line.TicketID,
				TicketTitle: line.TicketTitle,
				Sequence:    sequence,
				SeatID:      line.SeatID,
				Seat:        line.Seat,
			}, now)
			if err != nil {
				return nil, err
			}
			record, err := toRecord(item)
			if err != nil {
				return nil, err
			}
			issued = append(issued, *item)
			records = append(records, record)
		}
	}

	// One INSERT for the whole order. A festival order for twenty tickets is
	// one round trip, and all twenty commit with the payment or none do.
	//
	// Two different unique indexes can refuse this insert and they mean
	// opposite things: the order-line one means somebody else already issued
	// these and we should stand down, the code one means a draw collided and
	// we should try again. GORM translates both to the same
	// gorm.ErrDuplicatedKey and does not say which, so the two are told apart
	// by asking the question that actually matters, whether this order has
	// admissions now, rather than by parsing a constraint name out of a
	// driver error, which is the kind of string matching that breaks on a
	// driver upgrade.
	for attempt := 1; ; attempt++ {
		err := r.db.WithContext(ctx).Create(&records).Error
		if err == nil {
			return issued, nil
		}
		if !errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, err
		}

		var landed int64
		if countErr := r.db.WithContext(ctx).Model(&schema.Admission{}).
			Where("order_id = ?", order.OrderID).Count(&landed).Error; countErr != nil {
			return nil, countErr
		}
		if landed > 0 {
			// Another settlement of this order got there first. Not a failure:
			// the order has its tickets, they just are not ours to return.
			return nil, nil
		}
		if attempt >= codeCollisionAttempts {
			return nil, fmt.Errorf("issue admissions for order %s: %w", order.OrderID, err)
		}

		// Nothing landed, so the collision was on a code. Redraw every code in
		// the batch, the cheap thing, rather than work out which one lost.
		for index := range issued {
			code, codeErr := domain.NewCode()
			if codeErr != nil {
				return nil, codeErr
			}
			issued[index].Code = code
			record, recordErr := toRecord(&issued[index])
			if recordErr != nil {
				return nil, recordErr
			}
			records[index] = record
		}
	}
}

// FindByCode resolves a verified code through the blind index.
func (r *AdmissionRepository) FindByCode(ctx context.Context, code domain.Code) (*domain.Admission, error) {
	blind, err := piigorm.NewBlindIndex(schema.AdmissionCodeBlindScope, code.String())
	if err != nil {
		return nil, err
	}

	var record schema.Admission
	err = r.db.WithContext(ctx).Where("code_blind = ?", blind).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return toDomain(record), nil
}

// Admit spends an admission, and reports whether this call was the one.
//
// ONE statement. The WHERE clause carries the precondition, so the database
// decides who wins and there is no window between reading the status and
// writing it. Two turnstiles scanning the same ticket in the same millisecond
// produce one row affected and one zero, which is one person through.
func (r *AdmissionRepository) Admit(ctx context.Context, id, by string) (bool, error) {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&schema.Admission{}).
		Where("id = ? AND status = ?", id, string(domain.StatusIssued)).
		Updates(map[string]any{
			"status":      string(domain.StatusAdmitted),
			"admitted_at": now,
			"admitted_by": by,
			"updated_at":  now,
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// VoidForOrder withdraws every admission of an order.
//
// Admitted rows are voided too, and their admitted_at is left alone: somebody
// who came in and was refunded afterwards did both, and the attendance report
// still has to show they attended.
func (r *AdmissionRepository) VoidForOrder(ctx context.Context, orderID string) (int, error) {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&schema.Admission{}).
		Where("order_id = ? AND status <> ?", orderID, string(domain.StatusVoid)).
		Updates(map[string]any{
			"status":     string(domain.StatusVoid),
			"voided_at":  now,
			"updated_at": now,
		})
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

// ListByOrder is the holder's own tickets, codes decrypted, oldest tier first.
func (r *AdmissionRepository) ListByOrder(ctx context.Context, orderID string) ([]domain.Admission, error) {
	var records []schema.Admission
	if err := r.db.WithContext(ctx).
		Where("order_id = ?", orderID).
		Order("ticket_id ASC, sequence ASC").
		Find(&records).Error; err != nil {
		return nil, err
	}

	items := make([]domain.Admission, 0, len(records))
	for _, record := range records {
		items = append(items, *toDomain(record))
	}
	return items, nil
}

// CountAdmitted is the door's counter: how many are live and how many are in.
func (r *AdmissionRepository) CountAdmitted(ctx context.Context, eventID string) (int, int, error) {
	var row struct {
		Issued   int64
		Admitted int64
	}
	// One pass with conditional aggregates rather than two COUNTs, because the
	// door polls this and the index it uses is the same for both halves.
	err := r.db.WithContext(ctx).Raw(`
		SELECT
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS issued,
			COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS admitted
		FROM admissions
		WHERE event_id = ?`,
		string(domain.StatusIssued), string(domain.StatusAdmitted), eventID,
	).Scan(&row).Error
	if err != nil {
		return 0, 0, err
	}
	return int(row.Issued), int(row.Admitted), nil
}

// newID is an admission's identifier, in the prefixed-random convention every
// other table here uses.
func newID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "adm_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return "adm_" + hex.EncodeToString(buffer)
}

func toRecord(item *domain.Admission) (schema.Admission, error) {
	blind, err := piigorm.NewBlindIndex(schema.AdmissionCodeBlindScope, item.Code.String())
	if err != nil {
		return schema.Admission{}, fmt.Errorf("admission code blind index: %w", err)
	}
	return schema.Admission{
		ID:          item.ID,
		OrderID:     item.OrderID,
		EventID:     item.EventID,
		TicketID:    item.TicketID,
		TicketTitle: item.TicketTitle,
		Sequence:    item.Sequence,
		SeatID:      item.SeatID,
		SeatSection: item.Seat.Section,
		SeatRow:     item.Seat.Row,
		SeatLabel:   item.Seat.Seat,
		Code:        piigorm.NewEncrypted(item.Code.String()),
		CodeBlind:   blind,
		Status:      string(item.Status),
		AdmittedAt:  item.AdmittedAt,
		AdmittedBy:  item.AdmittedBy,
		VoidedAt:    item.VoidedAt,
		CreatedAt:   item.CreatedAt,
		UpdatedAt:   item.UpdatedAt,
	}, nil
}

func toDomain(record schema.Admission) *domain.Admission {
	return &domain.Admission{
		ID:          record.ID,
		OrderID:     record.OrderID,
		EventID:     record.EventID,
		TicketID:    record.TicketID,
		TicketTitle: record.TicketTitle,
		Sequence:    record.Sequence,
		SeatID:      record.SeatID,
		Seat: seatingdomain.Label{
			Section: record.SeatSection,
			Row:     record.SeatRow,
			Seat:    record.SeatLabel,
		},
		// Straight from the sealed column. It is NOT re-parsed: a code that
		// was valid when it was issued stays valid, and refusing to return a
		// stored code because today's checksum disagrees would lock a holder
		// out of a ticket they paid for.
		Code:       domain.Code(record.Code.String()),
		Status:     domain.Status(record.Status),
		AdmittedAt: record.AdmittedAt,
		AdmittedBy: record.AdmittedBy,
		VoidedAt:   record.VoidedAt,
		CreatedAt:  record.CreatedAt,
		UpdatedAt:  record.UpdatedAt,
	}
}
