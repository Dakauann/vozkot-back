package payment

import (
	"context"
	"testing"
	"time"

	ledgerdomain "vozkot/domain/ledger"
	orderdomain "vozkot/domain/order"
	"vozkot/infra/mercadopago"
	ledgerRepository "vozkot/infra/repositories/ledger"
	"vozkot/infra/testsupport"
	"vozkot/infra/uow"
	payoutUsecase "vozkot/usecases/payout"
)

// withLedger attaches organiser accrual to the shared payment harness.
//
// The harness is reused rather than copied: everything about settling a payment
// is already exercised by it, and the ledger is one more thing that has to
// commit with the same transaction. A second harness would be a second
// definition of "a paid order" that could drift from the first.
func withLedger(t *testing.T, h *harness) *ledgerRepository.LedgerRepository {
	t.Helper()
	h.service.WithLedger(payoutUsecase.NewService(
		uow.NewRunner(h.db),
		payoutUsecase.AlwaysStandard,
		ledgerdomain.BankingCalendar,
		time.Now,
		testsupport.Unique,
	))
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM ledger_entries WHERE organiser_id = ?", h.ownerID)
	})
	return ledgerRepository.NewLedgerRepository(h.db)
}

// pay drives an order all the way to paid through the real provider stub.
func (h *harness) pay(t *testing.T, item *orderdomain.Order) string {
	t.Helper()
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusPaid {
		t.Fatalf("order status = %q, want paid", stored.Status)
	}
	return paymentID
}

func TestAPaidOrderAccruesTheOrganisersShareAndNotTheFee(t *testing.T) {
	h := newHarness(t, 4)
	entries := withLedger(t, h)
	ctx := context.Background()

	item := h.pendingOrder(t, 2)
	h.pay(t, item)

	stored := h.order(t, item.ID)
	page, err := entries.List(ctx, ledgerdomain.Filter{OrderID: item.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("entries = %d, want a sale and a reserve", len(page.Items))
	}

	var accrued int64
	kinds := map[ledgerdomain.Kind]int64{}
	for _, entry := range page.Items {
		accrued += entry.AmountCents
		kinds[entry.Kind] = entry.AmountCents
		if entry.OrganiserID != h.ownerID {
			t.Errorf("entry credited %q, want the event's owner", entry.OrganiserID)
		}
	}
	// The organiser is owed their face value, the subtotal, and NOT the
	// buyer's total, because the service fee was added on top and was never
	// theirs. This is the assertion that would catch the whole ledger being
	// built on the wrong column.
	if accrued != stored.SubtotalCents {
		t.Fatalf("accrued %d against a subtotal of %d (buyer paid %d)",
			accrued, stored.SubtotalCents, stored.TotalCents)
	}
	if stored.BuyerFeeCents > 0 && accrued == stored.TotalCents {
		t.Fatal("the organiser accrued the buyer's fee as well as their own price")
	}
	// Standard terms: a tenth held back.
	if want := stored.SubtotalCents / 10; kinds[ledgerdomain.KindReserve] != want {
		t.Errorf("reserve = %d, want %d", kinds[ledgerdomain.KindReserve], want)
	}
	// Nothing is payable before the show, which is the whole premise.
	balance, err := entries.BalanceOf(ctx, h.ownerID, time.Now())
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if balance.AvailableCents != 0 {
		t.Errorf("available = %d before the event happened", balance.AvailableCents)
	}
	if balance.TotalCents != stored.SubtotalCents {
		t.Errorf("total = %d, want the whole face value %d", balance.TotalCents, stored.SubtotalCents)
	}
}

// A webhook delivered twice must not pay the organiser twice. This is the most
// expensive mistake the ledger could make, so it is the database's decision.
func TestARedeliveredPaymentAccruesOnce(t *testing.T) {
	h := newHarness(t, 4)
	entries := withLedger(t, h)
	ctx := context.Background()

	item := h.pendingOrder(t, 1)
	paymentID := h.pay(t, item)

	// The same approval again, exactly as the provider would resend it.
	for i := 0; i < 3; i++ {
		if err := h.service.SyncPayment(ctx, paymentID); err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
	}

	page, err := entries.List(ctx, ledgerdomain.Filter{OrderID: item.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("entries = %d after four deliveries, want the original two", len(page.Items))
	}
	balance, err := entries.BalanceOf(ctx, h.ownerID, time.Now())
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if balance.TotalCents != h.order(t, item.ID).SubtotalCents {
		t.Fatalf("balance = %d, want one order's worth", balance.TotalCents)
	}
}

// The safety property, end to end: a refunded order's claim is gone, and gone
// sooner than the sale it reverses would ever have become payable.
func TestARefundTakesTheClaimBackBeforeTheSaleWasEverPayable(t *testing.T) {
	h := newHarness(t, 4)
	entries := withLedger(t, h)
	ctx := context.Background()

	item := h.pendingOrder(t, 2)
	paymentID := h.pay(t, item)
	subtotal := h.order(t, item.ID).SubtotalCents

	h.provider.move(paymentID, mercadopago.StatusRefunded, "")
	if err := h.service.SyncPayment(ctx, paymentID); err != nil {
		t.Fatalf("SyncPayment(refunded): %v", err)
	}
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefunded {
		t.Fatalf("order status = %q, want refunded", stored.Status)
	}

	page, err := entries.List(ctx, ledgerdomain.Filter{OrderID: item.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var reversal *ledgerdomain.Entry
	for index, entry := range page.Items {
		if entry.Kind == ledgerdomain.KindRefund {
			reversal = &page.Items[index]
		}
	}
	if reversal == nil {
		t.Fatal("the refund left no entry on the ledger")
	}
	if reversal.AmountCents != -subtotal {
		t.Errorf("reversal = %d, want -%d", reversal.AmountCents, subtotal)
	}

	// Nothing is owed any more, at any point in time.
	balance, err := entries.BalanceOf(ctx, h.ownerID, time.Now())
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if balance.TotalCents != 0 {
		t.Fatalf("total = %d after a full refund, want nothing owed", balance.TotalCents)
	}
	// AT NO POINT is there anything to pay out. Walking forward past the
	// settlement date and past the reserve's release, the payable balance never
	// turns positive: the refund was available from the instant it was written
	// and the sale it reverses only became available later, so a payout run on
	// any of these days finds nothing owed.
	//
	// It IS negative in between, the sale has matured and the reserve has not,
	// and that negative is the design rather than a fault: the shortfall
	// sits visibly against money still being held.
	for _, days := range []int{1, 10, 40, 60, 120, 400} {
		future, err := entries.BalanceOf(ctx, h.ownerID, time.Now().AddDate(0, 0, days))
		if err != nil {
			t.Fatalf("BalanceOf(+%dd): %v", days, err)
		}
		if future.AvailableCents > 0 {
			t.Fatalf("+%dd: %d payable on a refunded order", days, future.AvailableCents)
		}
		if future.TotalCents != 0 {
			t.Fatalf("+%dd: total = %d, want nothing owed", days, future.TotalCents)
		}
	}
}

// A second refund on an order that already gave everything back must not write
// a second debit: that would take money off a balance twice for one event.
func TestAnAlreadyReversedOrderReversesNothingFurther(t *testing.T) {
	h := newHarness(t, 4)
	entries := withLedger(t, h)
	ctx := context.Background()

	item := h.pendingOrder(t, 1)
	paymentID := h.pay(t, item)
	h.provider.move(paymentID, mercadopago.StatusRefunded, "")
	for i := 0; i < 3; i++ {
		if err := h.service.SyncPayment(ctx, paymentID); err != nil {
			t.Fatalf("refund delivery %d: %v", i, err)
		}
	}

	page, err := entries.List(ctx, ledgerdomain.Filter{OrderID: item.ID, Kind: ledgerdomain.KindRefund})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("refund entries = %d, want exactly one", len(page.Items))
	}
	balance, err := entries.BalanceOf(ctx, h.ownerID, time.Now())
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if balance.TotalCents != 0 {
		t.Fatalf("total = %d, want zero and not a double reversal", balance.TotalCents)
	}
}

// The SQL aggregate and the pure function are two implementations of one
// definition, and this is the test that stops them drifting.
func TestTheDatabasesBalanceAgreesWithTheDomains(t *testing.T) {
	h := newHarness(t, 8)
	entries := withLedger(t, h)
	ctx := context.Background()

	paid := h.pendingOrder(t, 2)
	h.pay(t, paid)
	refunded := h.pendingOrder(t, 1)
	refundedPayment := h.pay(t, refunded)
	h.provider.move(refundedPayment, mercadopago.StatusRefunded, "")
	if err := h.service.SyncPayment(ctx, refundedPayment); err != nil {
		t.Fatalf("SyncPayment(refunded): %v", err)
	}

	page, err := entries.List(ctx, ledgerdomain.Filter{OrganiserID: h.ownerID, Limit: 200})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, at := range []time.Time{
		time.Now(),
		time.Now().AddDate(0, 0, 10),
		time.Now().AddDate(0, 0, 60),
		time.Now().AddDate(0, 0, 400),
	} {
		fromSQL, err := entries.BalanceOf(ctx, h.ownerID, at)
		if err != nil {
			t.Fatalf("BalanceOf(%s): %v", at, err)
		}
		if fromDomain := ledgerdomain.BalanceOf(page.Items, at); fromSQL != fromDomain {
			t.Fatalf("at %s the database says %+v and the domain says %+v", at, fromSQL, fromDomain)
		}
	}
}
