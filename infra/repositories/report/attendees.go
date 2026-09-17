package report

import (
	"context"
	"strings"
	"time"

	domain "vozkot/domain/report"

	"gorm.io/gorm"
)

// attendeeRow is one order line joined to its order.
type attendeeRow struct {
	OrderID        string
	PurchasedAt    *time.Time
	CreatedAt      time.Time
	Status         string
	BuyerName      string
	BuyerEmail     string
	BuyerDocument  string
	BuyerGender    string
	BuyerAgeYears  int
	BuyerCity      string
	BuyerUF        string
	TicketID       string
	TicketTitle    string
	Quantity       int
	UnitPriceCents int64
	TotalCents     int64
	AdmittedCount  int
}

// exportBatch is how many rows are read and written at a time.
//
// Big enough that an arena is a few hundred round trips rather than fifty
// thousand, small enough that the slice held in memory is measured in hundreds
// of kilobytes. The export streams, so this is the only thing resident at once.
const exportBatch = 500

// Attendees is the paginated list behind the organiser's table.
func (r *ReportRepository) Attendees(ctx context.Context, filter domain.AttendeeFilter) (domain.Page, error) {
	filter = filter.Normalize()

	var total int64
	if err := r.attendeeQuery(ctx, filter).Count(&total).Error; err != nil {
		return domain.Page{}, err
	}

	var rows []attendeeRow
	err := r.attendeeQuery(ctx, filter).
		Select(attendeeColumns).
		Order("o.created_at DESC, i.ticket_id").
		Limit(filter.Limit).
		Offset(filter.Offset).
		Scan(&rows).Error
	if err != nil {
		return domain.Page{}, err
	}

	items := make([]domain.Attendee, 0, len(rows))
	for index := range rows {
		items = append(items, toAttendee(&rows[index]))
	}
	return domain.Page{Items: items, Total: total}, nil
}

// StreamAttendees hands rows to fn in batches, for the export.
//
// Keyset pagination rather than OFFSET, and the reason matters at this size:
// OFFSET re-walks every row it skips, so exporting fifty thousand rows a
// page at a time is quadratic and the last page costs fifty thousand rows of
// work to return five hundred. Ordering by (created_at, order_id, ticket_id) —
// which is unique, because one order has one line per tier — lets each batch
// start exactly where the last one ended.
func (r *ReportRepository) StreamAttendees(
	ctx context.Context,
	filter domain.AttendeeFilter,
	fn func([]domain.Attendee) error,
) error {
	var (
		lastCreated  time.Time
		lastOrderID  string
		lastTicketID string
		written      int
	)

	for {
		query := r.attendeeQuery(ctx, filter).Select(attendeeColumns)
		if lastOrderID != "" {
			// Strictly after the last row in the previous batch, in the same
			// order the ORDER BY imposes.
			query = query.Where(
				"(o.created_at, o.id, i.ticket_id) > (?, ?, ?)",
				lastCreated, lastOrderID, lastTicketID,
			)
		}

		var rows []attendeeRow
		err := query.
			Order("o.created_at, o.id, i.ticket_id").
			Limit(exportBatch).
			Scan(&rows).Error
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}

		batch := make([]domain.Attendee, 0, len(rows))
		for index := range rows {
			batch = append(batch, toAttendee(&rows[index]))
		}
		if err := fn(batch); err != nil {
			return err
		}

		written += len(rows)
		if written >= domain.MaxExportRows {
			// The cap is honoured by stopping rather than by failing: an
			// organiser with more rows than this gets the first hundred
			// thousand and a filter to narrow, not an error and nothing.
			return nil
		}
		if len(rows) < exportBatch {
			return nil
		}
		last := rows[len(rows)-1]
		lastCreated, lastOrderID, lastTicketID = last.CreatedAt, last.OrderID, last.TicketID
	}
}

// attendeeColumns is shared so the list and the export cannot select different
// things and render differently.
const attendeeColumns = `
	o.id             AS order_id,
	o.paid_at        AS purchased_at,
	o.created_at     AS created_at,
	o.status         AS status,
	o.buyer_name     AS buyer_name,
	o.buyer_email    AS buyer_email,
	o.buyer_document AS buyer_document,
	o.buyer_gender   AS buyer_gender,
	o.buyer_age_years AS buyer_age_years,
	o.buyer_city     AS buyer_city,
	o.buyer_uf       AS buyer_uf,
	i.ticket_id      AS ticket_id,
	i.ticket_title   AS ticket_title,
	i.quantity       AS quantity,
	i.unit_price_cents AS unit_price_cents,
	-- i.total_cents is the LINE'S FACE VALUE, which is the organiser's.
	-- i.fee_cents is ours and is deliberately not selected: see the money
	-- note in domain/report.
	i.total_cents    AS total_cents,
	-- How many of this line's tickets have come through the door.
	--
	-- A correlated subquery rather than a join: a join onto admissions would
	-- multiply the order-item rows and every money column with them, which is
	-- the classic way an attendee export starts reporting three times the
	-- revenue. This reads the (order_id, ticket_id) index once per row.
	(
		SELECT COUNT(*) FROM admissions a
		WHERE a.order_id = i.order_id
		  AND a.ticket_id = i.ticket_id
		  AND a.admitted_at IS NOT NULL
	) AS admitted_count`

func (r *ReportRepository) attendeeQuery(ctx context.Context, filter domain.AttendeeFilter) *gorm.DB {
	query := r.db.WithContext(ctx).
		Table("order_items AS i").
		Joins("JOIN orders o ON o.id = i.order_id").
		Where("o.event_id = ?", strings.TrimSpace(filter.EventID))

	if status := strings.TrimSpace(filter.Status); status != "" {
		query = query.Where("o.status = ?", status)
	} else {
		// "All" still means all SALES. An attendee list that included expired
		// holds would have an organiser emailing people who never bought
		// anything.
		query = query.Where("o.status IN ?", soldStatuses())
	}
	if ticketID := strings.TrimSpace(filter.TicketID); ticketID != "" {
		query = query.Where("i.ticket_id = ?", ticketID)
	}
	if term := strings.TrimSpace(filter.Query); term != "" {
		// ILIKE on a name or an email, for finding one person at the door. It
		// is a scan within one event's rows, which is bounded by the event and
		// therefore affordable; a trigram index is the answer if a single event
		// ever makes it slow.
		pattern := "%" + strings.ToLower(term) + "%"
		query = query.Where("LOWER(o.buyer_name) LIKE ? OR LOWER(o.buyer_email) LIKE ?", pattern, pattern)
	}
	return query
}

func toAttendee(row *attendeeRow) domain.Attendee {
	purchased := row.CreatedAt
	if row.PurchasedAt != nil {
		purchased = *row.PurchasedAt
	}
	return domain.Attendee{
		OrderID:      row.OrderID,
		PurchasedAt:  purchased,
		Status:       row.Status,
		Name:         row.BuyerName,
		Email:        row.BuyerEmail,
		DocumentMask: MaskDocument(row.BuyerDocument),
		Gender:       row.BuyerGender,
		AgeYears:     row.BuyerAgeYears,
		City:         row.BuyerCity,
		UF:           row.BuyerUF,
		TicketID:     row.TicketID,
		TicketTitle:  row.TicketTitle,
		Quantity:     row.Quantity,

		UnitPriceCents: row.UnitPriceCents,
		NetCents:       row.TotalCents,
		AdmittedCount:  row.AdmittedCount,
	}
}

// MaskDocument shows enough to recognise a document and not enough to use one.
//
// First three and last two, which is the convention every Brazilian service
// uses for a CPF and what a person actually checks against their own card. The
// organiser needs to be able to match somebody to the ID they present at the
// door; they do not need to be able to be that person at a bank, so the whole
// number never leaves this layer.
//
// Exported because the CSV writer and the JSON DTO must mask identically — two
// maskings that differ by a digit is a leak in whichever one is looser.
func MaskDocument(document string) string {
	runes := []rune(strings.TrimSpace(document))
	if len(runes) == 0 {
		return ""
	}
	if len(runes) < 6 {
		return "***"
	}
	return string(runes[:3]) + "***" + string(runes[len(runes)-2:])
}
