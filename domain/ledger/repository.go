package ledger

import (
	"context"
	"time"
)

// Filter selects entries for a listing.
type Filter struct {
	OrganiserID string
	// EventID narrows to one show, which is the organiser's financial page.
	EventID string
	OrderID string
	Kind    Kind
	Limit   int
	Offset  int
}

// Page is a listing plus the total it was taken from.
type Page struct {
	Items []Entry
	Total int64
}

// Repository persists ledger entries.
//
// There is no Update and no Delete, and their absence is the point: an
// append-only table enforced by the port cannot be un-appended by a use case
// that means well. A correction is Append with the opposite sign.
type Repository interface {
	// Append writes entries, ignoring any whose id already exists.
	//
	// Idempotent on the entry id so the settle path can be retried: a payment
	// webhook arriving twice, a worker killed after the provider answered,
	// without paying an organiser twice for one order. That is the single most
	// expensive mistake this package could make, so it is the database's
	// decision and not a prior SELECT's.
	Append(ctx context.Context, entries []Entry) error
	// BalanceOf sums one organiser's position as of `now`.
	//
	// One aggregate query rather than reading rows into Go, because an
	// organiser with a year of sales has hundreds of thousands of them and the
	// answer is four numbers. It must agree with the pure BalanceOf in this
	// package; the repository's test is what proves it does.
	BalanceOf(ctx context.Context, organiserID string, now time.Time) (Balance, error)
	// AccruedOn reports what an order has already put on the ledger, split into
	// what it earned and what has since been taken back.
	//
	// Read before a reversal so a refund cannot exceed the accrual, and read
	// before an accrual so a retried settle writes nothing the second time.
	AccruedOn(ctx context.Context, orderID string) (Accrued, error)
	List(ctx context.Context, filter Filter) (Page, error)
}

// Accrued is one order's footprint on the ledger.
type Accrued struct {
	// EarnedCents is the sum of the positive entries: sale plus reserve.
	EarnedCents int64
	// ReversedCents is the absolute sum of refunds and chargebacks against it.
	ReversedCents int64
}

// Outstanding is what is still owed on this order, and therefore the most a
// further reversal may take.
func (a Accrued) Outstanding() int64 { return a.EarnedCents - a.ReversedCents }
