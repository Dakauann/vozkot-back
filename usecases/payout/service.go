// Package payout is the organiser's money: what they have earned, when it
// becomes theirs, and what a refund takes back off it.
//
// The arithmetic lives in domain/ledger and nothing is re-derived here. This
// package's job is the other half: reading the order and its event to learn WHO
// is owed and WHEN the show ends, deciding which terms apply, and writing the
// result inside the caller's transaction.
package payout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	eventdomain "vozkot/domain/event"
	ledgerdomain "vozkot/domain/ledger"
	orderdomain "vozkot/domain/order"
	uowdomain "vozkot/domain/uow"
)

// TermsFor decides which payout terms an organiser is on.
//
// A function so the risk tier can become a column, a table or a manual override
// without this package changing: today every organiser is standard, and the
// shape that replaces it is a lookup, not a rewrite.
type TermsFor func(ctx context.Context, organiserID string) ledgerdomain.Terms

// AlwaysStandard is the tier every organiser starts on.
func AlwaysStandard(context.Context, string) ledgerdomain.Terms { return ledgerdomain.StandardTerms }

type Service struct {
	uow      uowdomain.Runner
	terms    TermsFor
	calendar ledgerdomain.Calendar
	now      func() time.Time
	newID    func(prefix string) string
}

func NewService(
	unit uowdomain.Runner,
	terms TermsFor,
	calendar ledgerdomain.Calendar,
	now func() time.Time,
	newID func(prefix string) string,
) *Service {
	if terms == nil {
		terms = AlwaysStandard
	}
	if calendar.Holidays == nil {
		calendar.Holidays = ledgerdomain.NoHolidays
	}
	return &Service{uow: unit, terms: terms, calendar: calendar, now: now, newID: newID}
}

var (
	// ErrNoEvent is an order whose event has gone. The accrual is skipped
	// rather than failed: a settle path that refused to finish because the
	// catalogue lost a row would leave the buyer unpaid-for and un-ticketed
	// over a bookkeeping problem.
	ErrNoEvent = errors.New("payout: the order's event could not be read")
)

// Accrue writes what one paid order owes its organiser, inside the caller's
// transaction.
//
// Called from the settle path on the crossing into `paid`, for the same reason
// the admissions are issued there: the money and the record of the money have
// to commit together or a balance has to be rebuilt by hand.
//
// IDEMPOTENT BY CONSTRUCTION. The entry ids are derived from the order id, so a
// redelivered webhook computes the same ids and the repository's ON CONFLICT
// discards them. It does not read-then-write, because the race it would lose is
// exactly the one a duplicate webhook creates.
func (s *Service) Accrue(
	ctx context.Context,
	repositories uowdomain.Repositories,
	item *orderdomain.Order,
) error {
	if item == nil || item.SubtotalCents <= 0 {
		// A free event earns nothing and writes no rows. At volume that is a
		// table nobody wants full of zeroes.
		return nil
	}
	happening, err := repositories.Events().GetByID(ctx, item.EventID)
	if err != nil || happening == nil {
		return fmt.Errorf("%w: order %s, event %s", ErrNoEvent, item.ID, item.EventID)
	}
	sale := saleFrom(item, happening)
	entries, err := ledgerdomain.Accrue(
		sale, s.terms(ctx, sale.OrganiserID), s.now(), s.calendar, derivedIDs(item.ID),
	)
	if err != nil {
		return err
	}
	return repositories.Ledger().Append(ctx, entries)
}

// Reverse takes an order's claim back off the balance when its money goes back
// to the buyer.
//
// Bounded by what the order actually accrued rather than by what it is worth,
// so a second refund on a partly-reversed order cannot take more than is left,
// and so an order that never accrued (a free event, or one settled before
// this ledger existed) reverses nothing instead of writing a debit against a
// credit that was never there.
func (s *Service) Reverse(
	ctx context.Context,
	repositories uowdomain.Repositories,
	item *orderdomain.Order,
	kind ledgerdomain.Kind,
) error {
	if item == nil {
		return nil
	}
	accrued, err := repositories.Ledger().AccruedOn(ctx, item.ID)
	if err != nil {
		return err
	}
	outstanding := accrued.Outstanding()
	if outstanding <= 0 {
		return nil
	}
	happening, err := repositories.Events().GetByID(ctx, item.EventID)
	if err != nil || happening == nil {
		return fmt.Errorf("%w: order %s, event %s", ErrNoEvent, item.ID, item.EventID)
	}
	now := s.now()
	entry, err := ledgerdomain.Reverse(
		saleFrom(item, happening), kind, outstanding, outstanding, now,
		func() string { return s.newID("led") },
	)
	if err != nil {
		return err
	}
	return repositories.Ledger().Append(ctx, []ledgerdomain.Entry{entry})
}

// Balance is one organiser's position right now.
func (s *Service) Balance(ctx context.Context, organiserID string) (ledgerdomain.Balance, error) {
	var balance ledgerdomain.Balance
	err := s.uow.Run(ctx, func(ctx context.Context, repositories uowdomain.Repositories) error {
		var err error
		balance, err = repositories.Ledger().BalanceOf(ctx, organiserID, s.now())
		return err
	})
	return balance, err
}

// Entries lists an organiser's ledger, newest first.
func (s *Service) Entries(ctx context.Context, filter ledgerdomain.Filter) (ledgerdomain.Page, error) {
	var page ledgerdomain.Page
	err := s.uow.Run(ctx, func(ctx context.Context, repositories uowdomain.Repositories) error {
		var err error
		page, err = repositories.Ledger().List(ctx, filter)
		return err
	})
	return page, err
}

// saleFrom is the one place an order and its event are reduced to a ledger
// Sale. One function so the accrual and the reversal cannot disagree about
// which number is the organiser's share or when the show ends.
func saleFrom(item *orderdomain.Order, happening *eventdomain.Event) ledgerdomain.Sale {
	// A single-night show has no stated end, and its start is the honest
	// anchor: settlement counts from when the doors closed, and for a one-night
	// event that is the night it began.
	endsAt := happening.StartsAt
	if happening.EndsAt != nil && happening.EndsAt.After(endsAt) {
		endsAt = *happening.EndsAt
	}
	return ledgerdomain.Sale{
		OrderID:     item.ID,
		EventID:     item.EventID,
		OrganiserID: happening.OwnerID,
		// The organiser's share, never the buyer's total: the service fee was
		// added on top of their price and is not theirs to be owed.
		FaceCents:   item.SubtotalCents,
		EventEndsAt: endsAt,
	}
}

// derivedIDs numbers an order's accrual entries from the order itself.
//
// Deterministic on purpose: it is what makes Append idempotent without a read,
// so a webhook delivered twice, or a worker reclaimed after the provider
// answered, writes one accrual. A random id would make the second delivery a
// second payment.
//
// HASHED RATHER THAN CONCATENATED, and that is not a detail. The obvious
// "led_<orderID>_<n>" is the same length as the order id plus a counter, and
// an id column is varchar(32): it fit for a production order id and overflowed
// the moment a longer one appeared, which is a settle path failing on an INSERT
// after the money had already moved. A digest is 28 characters whatever it is
// given.
func derivedIDs(orderID string) func() string {
	n := 0
	return func() string {
		n++
		sum := sha256.Sum256([]byte(orderID + ":" + strconv.Itoa(n)))
		return "led_" + hex.EncodeToString(sum[:12])
	}
}
