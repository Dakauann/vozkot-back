// Package report reads the organiser's numbers out of PostgreSQL.
//
// Every breakdown is a GROUP BY over columns frozen onto the order at purchase,
// which is what makes this package possible at all: the buyer's own profile is
// encrypted and could not be grouped without decrypting the whole table, while
// the snapshot is coarse, plaintext and indexed.
//
// One rule runs through all the SQL below and it is the only subtle thing here.
// A breakdown needs numbers at two different GRAINS: money and order counts
// belong to the ORDER, ticket counts belong to its ITEMS. Joining the two and
// aggregating in one pass multiplies an order's money by the number of tiers on
// it, a two-tier order counted twice, which is the classic fan-out. So every
// query folds the items to one row per order FIRST (the `counted` CTE) and only
// then joins, which keeps each order contributing its money exactly once.
package report

import (
	"context"
	"fmt"
	"strings"
	"time"

	domain "vozkot/domain/report"

	"gorm.io/gorm"
)

type ReportRepository struct {
	db *gorm.DB
}

func NewReportRepository(db *gorm.DB) *ReportRepository {
	return &ReportRepository{db: db}
}

var _ domain.Repository = (*ReportRepository)(nil)

// soldStatuses are the orders that count as a sale.
//
// Paid and refunded, and including refunded is the interesting half: an
// organiser asking "who bought" needs the refunds visible rather than silently
// missing, and the totals report them separately instead of deducting them.
// Holds, expiries and failures were never sales and never appear.
func soldStatuses() []string { return []string{"paid", "refunded"} }

// cityLimit bounds the city breakdown.
//
// Brazil has 5,570 municipalities and a national tour reaches hundreds of them.
// A table with a row per city is not a report anybody reads, and sending it is
// a response that grows without limit, so the biggest are returned and the tail
// is the organiser's cue to look at the state breakdown instead.
const cityLimit = 50

// Sales computes every breakdown for one event.
//
// Six queries rather than one. A single query producing six groupings needs
// either six passes in a CTE or a GROUPING SETS that returns rows nobody can
// read, and each of these is an index scan over one event's orders. They run in
// sequence on purpose: they share one pooled connection sized for checkout
// traffic, and a reporting screen must never be able to take six of those at
// once.
func (r *ReportRepository) Sales(ctx context.Context, scope domain.Scope) (domain.Sales, error) {
	scope.EventID = strings.TrimSpace(scope.EventID)
	scope.OrganiserID = strings.TrimSpace(scope.OrganiserID)
	sales := domain.Sales{EventID: scope.EventID}
	// An invalid scope returns nothing rather than everything. See Scope.Valid.
	if !scope.Valid() {
		return sales, nil
	}

	totals, err := r.totals(ctx, scope)
	if err != nil {
		return domain.Sales{}, err
	}
	sales.Totals = totals

	if sales.ByGender, err = r.groupBy(ctx, scope, "o.buyer_gender", 0); err != nil {
		return domain.Sales{}, err
	}
	if sales.ByUF, err = r.groupBy(ctx, scope, "o.buyer_uf", 0); err != nil {
		return domain.Sales{}, err
	}
	if sales.ByCity, err = r.groupBy(ctx, scope, "o.buyer_city", cityLimit); err != nil {
		return domain.Sales{}, err
	}
	if sales.ByAge, err = r.groupBy(ctx, scope, ageBucketSQL, 0); err != nil {
		return domain.Sales{}, err
	}
	if sales.ByTier, err = r.byTier(ctx, scope); err != nil {
		return domain.Sales{}, err
	}
	if sales.ByDay, err = r.byDay(ctx, scope); err != nil {
		return domain.Sales{}, err
	}
	return sales, nil
}

// scopeClause is the WHERE fragment and its arguments for one report scope.
//
// The single place the difference between "this event" and "everything this
// organiser sells" lives. Every breakdown below interpolates it, so a portfolio
// total and the sum of its events are computed by the same SQL with one clause
// swapped, which is what stops them drifting apart.
//
// The organiser form is a subquery on events rather than a join, deliberately:
// a join would fan the orders out again and every aggregate here is built
// around not doing that. `owner_id` is indexed, and the planner turns the
// subquery into a hash semi-join over that index.
func scopeClause(scope domain.Scope) (string, []any) {
	if scope.EventID != "" {
		return "o.event_id = ?", []any{scope.EventID}
	}
	return "o.event_id IN (SELECT e.id FROM events e WHERE e.owner_id = ?)", []any{scope.OrganiserID}
}

// ageBucketSQL mirrors domain/report.BracketFor exactly.
//
// Bucketed in SQL because the alternative is reading every order of the event
// into memory to count them. The duplication is real and is held together by
// TestSQLBucketsMatchTheDomain in this package, which walks every age through
// both: a boundary that differed would put the same person in two different
// bands depending on which code path asked.
const ageBucketSQL = `
	CASE
		WHEN o.buyer_age_years <= 0  THEN 'unknown'
		WHEN o.buyer_age_years <= 18 THEN 'up_to_18'
		WHEN o.buyer_age_years <= 23 THEN '19_23'
		WHEN o.buyer_age_years <= 28 THEN '24_28'
		WHEN o.buyer_age_years <= 33 THEN '29_33'
		WHEN o.buyer_age_years <= 38 THEN '34_38'
		WHEN o.buyer_age_years <= 43 THEN '39_43'
		WHEN o.buyer_age_years <= 48 THEN '44_48'
		WHEN o.buyer_age_years <= 53 THEN '49_53'
		WHEN o.buyer_age_years <= 58 THEN '54_58'
		ELSE '59_plus'
	END`

// groupedExpressions is the allow-list of things groupBy may group on.
//
// The expression is interpolated into SQL rather than bound, because a column
// name cannot be a bind parameter. Every caller in this file passes a literal,
// and this list makes that guarantee mechanical rather than a promise: an
// expression not on it produces no query at all, so a future caller that wires
// a request parameter through here gets an empty result instead of an
// injection.
var groupedExpressions = map[string]bool{
	"o.buyer_gender": true,
	"o.buyer_uf":     true,
	"o.buyer_city":   true,
	ageBucketSQL:     true,
}

// groupBy is the body of every order-grained breakdown.
//
// limit of 0 means all rows.
func (r *ReportRepository) groupBy(ctx context.Context, scope domain.Scope, expression string, limit int) ([]domain.Slice, error) {
	if !groupedExpressions[expression] {
		return nil, fmt.Errorf("report: refusing to group by an unregistered expression")
	}

	// The items are folded to one row per order BEFORE the join, so each order
	// contributes its money once however many tiers it spans.
	where, args := scopeClause(scope)
	query := `
		WITH sold AS (
			SELECT o.id, o.subtotal_cents,
			       ` + expression + ` AS bucket
			FROM orders o
			WHERE ` + where + ` AND o.status IN ?
		),
		counted AS (
			SELECT s.id, COALESCE(SUM(i.quantity), 0) AS tickets
			FROM sold s
			LEFT JOIN order_items i ON i.order_id = s.id
			GROUP BY s.id
		)
		SELECT
			s.bucket                                AS key,
			COUNT(*)                                AS orders,
			COALESCE(SUM(c.tickets), 0)             AS tickets,
			-- Net only. The commission is not selected here, and that
			-- absence is the control: see the money note in domain/report.
			COALESCE(SUM(s.subtotal_cents), 0)      AS net_cents
		FROM sold s
		JOIN counted c ON c.id = s.id
		GROUP BY s.bucket
		ORDER BY tickets DESC, orders DESC`
	if limit > 0 {
		query += fmt.Sprintf("\n\t\tLIMIT %d", limit)
	}

	var rows []sliceRow
	if err := r.db.WithContext(ctx).Raw(query, append(args, soldStatuses())...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return toSlices(rows), nil
}

// totals is the headline, and the denominator every share on the page uses.
func (r *ReportRepository) totals(ctx context.Context, scope domain.Scope) (domain.Totals, error) {
	var row struct {
		Orders   int64
		Buyers   int64
		Tickets  int64
		NetCents int64
	}
	where, args := scopeClause(scope)
	err := r.db.WithContext(ctx).Raw(`
		WITH sold AS (
			SELECT o.id, o.buyer_id, o.subtotal_cents
			FROM orders o
			WHERE `+where+` AND o.status IN ?
		),
		counted AS (
			SELECT s.id, COALESCE(SUM(i.quantity), 0) AS tickets
			FROM sold s
			LEFT JOIN order_items i ON i.order_id = s.id
			GROUP BY s.id
		)
		SELECT
			COUNT(*)                                        AS orders,
			-- DISTINCT accounts, which is what an organiser means by "how many
			-- people". NULLIF drops the door sales that have no account behind
			-- them rather than counting them all as one person.
			COUNT(DISTINCT NULLIF(s.buyer_id, ''))          AS buyers,
			COALESCE(SUM(c.tickets), 0)                     AS tickets,
			COALESCE(SUM(s.subtotal_cents), 0)              AS net_cents
		FROM sold s
		JOIN counted c ON c.id = s.id`,
		append(args, soldStatuses())...,
	).Scan(&row).Error
	if err != nil {
		return domain.Totals{}, err
	}

	var refunded struct {
		Orders int64
		Cents  int64
	}
	err = r.db.WithContext(ctx).Raw(`
		-- subtotal_cents and NOT total_cents. A refund returns our fee to
		-- the buyer as well, so a refunded figure at gross sitting beside net
		-- sales would hand the organiser our commission by subtraction.
		SELECT COUNT(*) AS orders, COALESCE(SUM(subtotal_cents), 0) AS cents
		FROM orders o
		WHERE `+where+` AND o.status = 'refunded'`, args...,
	).Scan(&refunded).Error
	if err != nil {
		return domain.Totals{}, err
	}

	return domain.Totals{
		Orders:         int(row.Orders),
		Tickets:        int(row.Tickets),
		Buyers:         int(row.Buyers),
		NetCents:       row.NetCents,
		RefundedOrders: int(refunded.Orders),
		RefundedCents:  refunded.Cents,
	}, nil
}

// byTier is the one item-grained breakdown, and the only one whose money can be
// summed straight off the join: an order_item's totals belong to that line, so
// there is no fan-out to avoid.
func (r *ReportRepository) byTier(ctx context.Context, scope domain.Scope) ([]domain.Slice, error) {
	var rows []sliceRow
	where, args := scopeClause(scope)
	err := r.db.WithContext(ctx).Raw(`
		SELECT
			i.ticket_id                                   AS key,
			MIN(i.ticket_title)                           AS label,
			COUNT(DISTINCT i.order_id)                    AS orders,
			COALESCE(SUM(i.quantity), 0)                  AS tickets,
			COALESCE(SUM(i.total_cents + i.fee_cents), 0) AS gross_cents,
			COALESCE(SUM(i.total_cents), 0)               AS net_cents,
			COALESCE(SUM(i.fee_cents), 0)                 AS fee_cents
		FROM order_items i
		JOIN orders o ON o.id = i.order_id
		WHERE `+where+` AND o.status IN ?
		GROUP BY i.ticket_id
		ORDER BY tickets DESC`, append(args, soldStatuses())...,
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return toSlices(rows), nil
}

// byDay is the sales curve, grouped on the day the money LANDED.
//
// paid_at and not created_at: a basket opened on Monday and paid on Thursday is
// Thursday's revenue, and a curve drawn on creation counts holds that never
// became money. An order with no paid_at cannot appear, which is correct: it
// was never a sale.
func (r *ReportRepository) byDay(ctx context.Context, scope domain.Scope) ([]domain.DaySlice, error) {
	var rows []struct {
		Day      string
		Orders   int64
		Tickets  int64
		NetCents int64
	}
	where, args := scopeClause(scope)
	err := r.db.WithContext(ctx).Raw(`
		WITH sold AS (
			SELECT
				date_trunc('day', o.paid_at AT TIME ZONE 'UTC') AS day,
				o.id, o.subtotal_cents
			FROM orders o
			WHERE `+where+` AND o.status IN ? AND o.paid_at IS NOT NULL
		),
		counted AS (
			SELECT s.id, COALESCE(SUM(i.quantity), 0) AS tickets
			FROM sold s
			LEFT JOIN order_items i ON i.order_id = s.id
			GROUP BY s.id
		)
		SELECT
			to_char(s.day, 'YYYY-MM-DD')          AS day,
			COUNT(*)                              AS orders,
			COALESCE(SUM(c.tickets), 0)           AS tickets,
			COALESCE(SUM(s.subtotal_cents), 0)    AS net_cents
		FROM sold s
		JOIN counted c ON c.id = s.id
		GROUP BY s.day
		ORDER BY s.day`, append(args, soldStatuses())...,
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	days := make([]domain.DaySlice, 0, len(rows))
	for _, row := range rows {
		parsed, err := time.Parse("2006-01-02", row.Day)
		if err != nil {
			return nil, fmt.Errorf("report: unreadable day %q: %w", row.Day, err)
		}
		days = append(days, domain.DaySlice{
			Day:      parsed,
			Orders:   int(row.Orders),
			Tickets:  int(row.Tickets),
			NetCents: row.NetCents,
		})
	}
	return days, nil
}

// sliceRow is the shape every breakdown query scans into.
type sliceRow struct {
	Key      string
	Label    string
	Orders   int64
	Tickets  int64
	NetCents int64
}

func toSlices(rows []sliceRow) []domain.Slice {
	slices := make([]domain.Slice, 0, len(rows))
	for _, row := range rows {
		key := strings.TrimSpace(row.Key)
		if key == "" {
			// An empty column is "not informed", folded into the same bucket a
			// buyer who chose "prefiro não informar" lands in: to a count of an
			// audience, the two mean the same thing.
			key = domain.UnknownKey
		}
		slices = append(slices, domain.Slice{
			Key:      key,
			Label:    row.Label,
			Orders:   int(row.Orders),
			Tickets:  int(row.Tickets),
			NetCents: row.NetCents,
		})
	}
	return slices
}
