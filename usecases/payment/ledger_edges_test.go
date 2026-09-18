package payment

import (
	"context"
	"sync"
	"testing"
	"time"

	ledgerdomain "vozkot/domain/ledger"
	orderdomain "vozkot/domain/order"
	uowdomain "vozkot/domain/uow"
	"vozkot/infra/mercadopago"
	"vozkot/infra/testsupport"
	"vozkot/infra/uow"
	payoutUsecase "vozkot/usecases/payout"
)

// A bank pulling money back is not the same event as a buyer changing their
// mind, even though both leave the order `refunded`.
//
// The distinction is load-bearing twice over: the reserve exists specifically
// for Pix reversals, and the Terms bill the acquirer's cost to different
// parties for the two. A statement that called both "refund" would have lost
// the only thing telling them apart.
func TestAChargebackIsRecordedAsAChargebackAndNotAsARefund(t *testing.T) {
	h := newHarness(t, 4)
	entries := withLedger(t, h)
	ctx := context.Background()

	item := h.pendingOrder(t, 1)
	paymentID := h.pay(t, item)
	subtotal := h.order(t, item.ID).SubtotalCents

	h.provider.move(paymentID, mercadopago.StatusChargedBack, "")
	if err := h.service.SyncPayment(ctx, paymentID); err != nil {
		t.Fatalf("SyncPayment(charged_back): %v", err)
	}
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefunded {
		t.Fatalf("order status = %q, want refunded", stored.Status)
	}

	page, err := entries.List(ctx, ledgerdomain.Filter{OrderID: item.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var kinds []ledgerdomain.Kind
	for _, entry := range page.Items {
		kinds = append(kinds, entry.Kind)
		if entry.Kind == ledgerdomain.KindChargeback && entry.AmountCents != -subtotal {
			t.Errorf("chargeback = %d, want -%d", entry.AmountCents, subtotal)
		}
	}
	var chargebacks, refunds int
	for _, kind := range kinds {
		switch kind {
		case ledgerdomain.KindChargeback:
			chargebacks++
		case ledgerdomain.KindRefund:
			refunds++
		}
	}
	if chargebacks != 1 || refunds != 0 {
		t.Fatalf("kinds = %v, want exactly one chargeback and no refund", kinds)
	}
}

// The race the idempotency is actually FOR.
//
// The redelivery test fires sequentially, which proves the second call is a
// no-op but not that two in flight at once are. This is the shape a provider
// really produces, a retry overlapping the original, and it is the one where
// a read-then-write would write two accruals and pay an organiser twice.
func TestConcurrentDeliveriesOfOnePaymentAccrueOnce(t *testing.T) {
	h := newHarness(t, 8)
	entries := withLedger(t, h)
	ctx := context.Background()

	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)

	const deliveries = 6
	var wg sync.WaitGroup
	errs := make(chan error, deliveries)
	start := make(chan struct{})
	for i := 0; i < deliveries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // released together, so they genuinely overlap
			errs <- h.service.SyncPayment(ctx, paymentID)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	// Some deliveries may lose the row lock and return an error; what must not
	// happen is two accruals. A failed delivery is retried by the queue.
	for err := range errs {
		_ = err
	}

	page, err := entries.List(ctx, ledgerdomain.Filter{OrderID: item.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("entries = %d after %d concurrent deliveries, want the original two",
			len(page.Items), deliveries)
	}
	balance, err := entries.BalanceOf(ctx, h.ownerID, time.Now())
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if want := h.order(t, item.ID).SubtotalCents; balance.TotalCents != want {
		t.Fatalf("balance = %d after concurrent deliveries, want one order's worth %d",
			balance.TotalCents, want)
	}
}

// A free event accrues nothing, and the guard is tested where it can actually
// be reached.
//
// Not through the payment path: a charge of zero is refused by the gateway
// before settle() is ever called, so a free order never crosses into `paid`
// that way at all. The guard still matters: a future flow that confirms a free
// RSVP without a charge would reach Accrue directly, and ten per cent of
// nothing written as a row is a table full of zeroes nobody wants at volume.
func TestAFreeOrderAccruesNothing(t *testing.T) {
	h := newHarness(t, 4)
	entries := withLedger(t, h)
	ctx := context.Background()

	item := h.pendingOrder(t, 1)
	free := *item
	free.SubtotalCents = 0

	payouts := payoutUsecase.NewService(
		uow.NewRunner(h.db), payoutUsecase.AlwaysStandard,
		ledgerdomain.BankingCalendar, time.Now, testsupport.Unique,
	)
	err := uow.NewRunner(h.db).Run(ctx, func(ctx context.Context, repositories uowdomain.Repositories) error {
		return payouts.Accrue(ctx, repositories, &free)
	})
	if err != nil {
		t.Fatalf("Accrue on a free order: %v", err)
	}

	page, listErr := entries.List(ctx, ledgerdomain.Filter{OrderID: item.ID})
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(page.Items) != 0 {
		t.Fatalf("a free order wrote %d ledger entries: %+v", len(page.Items), page.Items)
	}
}

// An order refunded before it was ever paid accrued nothing, so it must reverse
// nothing. `refund_required` is exactly this: a payment that landed after the
// hold lapsed and the seat had been resold.
func TestAnOrderThatNeverAccruedReversesNothing(t *testing.T) {
	h := newHarness(t, 4)
	entries := withLedger(t, h)
	ctx := context.Background()

	item := h.pendingOrder(t, 1)
	paymentID := h.charge(t, item)
	// Straight to refunded without ever crossing into paid.
	h.provider.move(paymentID, mercadopago.StatusRefunded, "")
	if err := h.service.SyncPayment(ctx, paymentID); err != nil {
		t.Fatalf("SyncPayment(refunded): %v", err)
	}

	page, err := entries.List(ctx, ledgerdomain.Filter{OrderID: item.ID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, entry := range page.Items {
		if entry.AmountCents < 0 {
			t.Fatalf("a debit was written against an order that never accrued: %+v", entry)
		}
	}
	balance, err := entries.BalanceOf(ctx, h.ownerID, time.Now())
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	if balance.TotalCents < 0 {
		t.Fatalf("balance = %d, want never negative from an unpaid order", balance.TotalCents)
	}
}
