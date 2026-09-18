package payment

import (
	"context"
	"testing"

	"vozkot/infra/mercadopago"

	orderdomain "vozkot/domain/order"
)

// Cancelling the charge when the hold lapses.
//
// THE WINDOW THIS CLOSES: a hold is thirty minutes and a provider dates a
// charge to a DAY, so releasing an order's stock used to leave its PIX code
// payable for another twenty-odd hours. Anybody who paid in that window sent
// real money for seats already back on sale, and the only remaining move was to
// take it and hand it straight back, losing the provider's fee.
//
// CancelCharge existed on both adapters and on the port from the beginning, and
// nothing in the system ever called it.

func TestAnExpiredHoldVoidsItsCharge(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()

	item := h.pendingOrder(t, 1)
	paymentID := h.charge(t, item)
	h.expire(t, item)

	if err := h.service.CancelCharge(ctx, item.ID); err != nil {
		t.Fatalf("CancelCharge() error = %v", err)
	}
	if status := h.provider.status(paymentID); status != mercadopago.StatusCancelled {
		t.Errorf("charge %s is %q, want cancelled: the code is still payable for seats that went back on sale",
			paymentID, status)
	}
}

// The race that makes this a status check rather than a flag: the job is
// written when the hold lapses, and a buyer may pay in the seconds before it
// runs. Voiding then would take away tickets somebody legitimately holds.
func TestAPaidOrderIsNeverVoided(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()

	item := h.pendingOrder(t, 1)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(ctx, paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusPaid {
		t.Fatalf("status = %q, want paid", stored.Status)
	}

	// The cancellation job, arriving late.
	if err := h.service.CancelCharge(ctx, item.ID); err != nil {
		t.Fatalf("CancelCharge() error = %v", err)
	}

	if status := h.provider.status(paymentID); status != mercadopago.StatusApproved {
		t.Errorf("a PAID charge was voided (%q): the buyer holds valid tickets", status)
	}
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusPaid {
		t.Errorf("order moved to %q after a late cancellation", stored.Status)
	}
}

// Most abandoned carts never reach a provider at all: the hold lapses before
// anybody confirms. That is the common path, not an error.
func TestAnOrderWithNoChargeCancelsNothing(t *testing.T) {
	h := newHarness(t, 5)
	item := h.pendingOrder(t, 1)
	h.expire(t, item)

	if err := h.service.CancelCharge(context.Background(), item.ID); err != nil {
		t.Fatalf("CancelCharge() error = %v, want a quiet no-op", err)
	}
}

// Sweeps overlap and providers redeliver; voiding twice must be as harmless as
// voiding once.
func TestCancellingTwiceIsHarmless(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()

	item := h.pendingOrder(t, 1)
	paymentID := h.charge(t, item)
	h.expire(t, item)

	for attempt := 0; attempt < 3; attempt++ {
		if err := h.service.CancelCharge(ctx, item.ID); err != nil {
			t.Fatalf("CancelCharge() attempt %d error = %v", attempt, err)
		}
	}
	if status := h.provider.status(paymentID); status != mercadopago.StatusCancelled {
		t.Errorf("charge %s is %q after three cancellations", paymentID, status)
	}
}
