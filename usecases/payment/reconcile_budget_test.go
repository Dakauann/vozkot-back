package payment

import (
	"context"
	"testing"
	"time"

	orderdomain "vozkot/domain/order"
	queuedomain "vozkot/domain/queue"
)

// What reconciliation COSTS, counted rather than argued.
//
// THE PROBLEM: the sweep re-read every pending order from the provider on every
// pass. One unpaid order therefore cost a provider call a minute for the whole
// length of its hold, and an onsale full of people who had not opened their
// bank app yet spent thousands of calls discovering exactly that.
//
// It is not an abstract waste. Asaas caps an account at 25,000 API requests per
// twelve hours, across all endpoints, and its documentation says the cap can be
// raised for genuine charge generation but NOT for accounts that poll. Mercado
// Pago publishes no number at all and answers 429 when it decides you have had
// enough. Both punish this shape.
//
// The fix is a staleness filter, not a slower sweep: settlement refreshes
// UpdatedAt even when the charge has not moved, so each read pushes the next
// one out and the backoff needs no state of its own.

// sweepFor runs the reconcile sweep once a minute for the length of a hold,
// simulating the worker draining what it schedules, and returns how many
// provider reads that would have cost for one order.
func (h *harness) sweepFor(t *testing.T, orderID string, hold, every time.Duration) int {
	t.Helper()
	ctx := context.Background()
	clock := time.Now().UTC()
	h.service.now = func() time.Time { return clock }

	reads := 0
	for elapsed := time.Duration(0); elapsed < hold; elapsed += every {
		clock = clock.Add(every)
		if _, err := h.service.Reconcile(ctx, 100); err != nil {
			t.Fatalf("Reconcile() at +%s: %v", elapsed, err)
		}
		// Drain: every scheduled job is one provider read, and running it
		// refreshes the order the way settle() does for a charge that has not
		// moved.
		var scheduled int64
		if err := h.db.Table("jobs").
			Where("type = ? AND payload->>'orderId' = ? AND status = ?",
				queuedomain.TypeSyncPayment, orderID, string(queuedomain.StatusPending)).
			Count(&scheduled).Error; err != nil {
			t.Fatalf("count scheduled: %v", err)
		}
		if scheduled == 0 {
			continue
		}
		reads += int(scheduled)
		if err := h.db.Exec("DELETE FROM jobs WHERE type = ? AND payload->>'orderId' = ?",
			queuedomain.TypeSyncPayment, orderID).Error; err != nil {
			t.Fatalf("drain jobs: %v", err)
		}
		if err := h.db.Exec("UPDATE orders SET updated_at = ? WHERE id = ?", clock, orderID).Error; err != nil {
			t.Fatalf("refresh the order: %v", err)
		}
	}
	return reads
}

// The headline number: one abandoned cart, over one thirty-minute hold.
func TestAnUnpaidOrderDoesNotCostAProviderCallAMinute(t *testing.T) {
	h := newHarness(t, 5)
	item := h.pendingOrder(t, 1)
	h.charge(t, item)

	reads := h.sweepFor(t, item.ID, 30*time.Minute, time.Minute)

	// Thirty one-minute sweeps over a three-minute staleness window is ten
	// reads, not thirty.
	if reads > 11 {
		t.Fatalf("one unpaid order cost %d provider reads over a 30-minute hold; "+
			"with a %s staleness window it should cost about 10. The sweep is "+
			"polling again.", reads, DefaultReconcileAfter)
	}
	if reads == 0 {
		t.Fatal("the sweep never re-read a pending order: a lost webhook would " +
			"strand the buyer forever")
	}
	t.Logf("one unpaid order over a 30-minute hold: %d provider reads (was 30)", reads)
}

// The recovery guarantee, which is what the threshold trades against: a lost
// webhook must still be caught, and within the window rather than eventually.
func TestALostWebhookIsStillCaughtWithinTheWindow(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()
	item := h.pendingOrder(t, 1)
	h.charge(t, item)

	clock := time.Now().UTC()
	h.service.now = func() time.Time { return clock }

	// Nothing is due yet: the order was just touched.
	scheduled, err := h.service.Reconcile(ctx, 100)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if scheduled != 0 {
		t.Errorf("a freshly-read order was re-read immediately (%d scheduled)", scheduled)
	}

	// Past the window, it is.
	clock = clock.Add(DefaultReconcileAfter + time.Minute)
	scheduled, err = h.service.Reconcile(ctx, 100)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if scheduled == 0 {
		t.Fatalf("an order untouched for %s was never re-read: a lost webhook "+
			"would strand the buyer", DefaultReconcileAfter+time.Minute)
	}
}

// The threshold is configurable, because the trade is deployment-specific: a
// shorter window recovers a lost webhook sooner and costs more calls.
func TestTheStalenessWindowIsConfigurable(t *testing.T) {
	h := newHarness(t, 5)
	h.service.WithReconcileAfter(20 * time.Minute)
	item := h.pendingOrder(t, 1)
	h.charge(t, item)

	reads := h.sweepFor(t, item.ID, 30*time.Minute, time.Minute)
	if reads > 2 {
		t.Errorf("a 20-minute window cost %d reads over a 30-minute hold, want at most 2", reads)
	}
}

// A paid order leaves the pending set, so it stops costing anything at all.
// This is why the sweep is cheap in practice: with webhooks working, most
// orders are never re-read even once.
func TestASettledOrderIsNeverSweptAgain(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()
	item := h.pendingOrder(t, 1)
	h.charge(t, item)

	if err := h.db.Exec("UPDATE orders SET status = ?, updated_at = NOW() - INTERVAL '1 hour' WHERE id = ?",
		string(orderdomain.StatusPaid), item.ID).Error; err != nil {
		t.Fatalf("settle the order: %v", err)
	}

	scheduled, err := h.service.Reconcile(ctx, 100)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var jobs int64
	if err := h.db.Table("jobs").
		Where("type = ? AND payload->>'orderId' = ?", queuedomain.TypeSyncPayment, item.ID).
		Count(&jobs).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 0 {
		t.Errorf("a paid order was scheduled for reconciliation (%d jobs, %d scheduled this pass)", jobs, scheduled)
	}
}
