package payment

import (
	"context"
	"testing"

	refunddomain "vozkot/domain/refund"
	"vozkot/infra/mercadopago"
	eventRepository "vozkot/infra/repositories/event"
	refundRepository "vozkot/infra/repositories/refund"
	"vozkot/infra/uow"
	queueUsecase "vozkot/usecases/queue"
	refundUsecase "vozkot/usecases/refund"

	orderdomain "vozkot/domain/order"
	queuedomain "vozkot/domain/queue"
)

// Settlement pays back what it cannot deliver.
//
// THE GAP THIS CLOSES: a payment arriving after its hold lapsed, for seats
// already resold, moved the order to refund_required and stopped there. No
// request, no job, no message: one log line, and the money stayed with the box
// office until somebody thought to query for that status. The documented
// promise is that this refund is full, immediate and automatic, because our own
// timing caused it, and nothing implemented it.
//
// The refund has to commit WITH the settlement that owed it, or there is a
// window in which the system has recorded a debt and done nothing about it.

// withRefunds attaches a real refund service, wired exactly as the container
// wires it: built from the payment service, then handed back to it.
func (h *harness) withRefunds(t *testing.T) refunddomain.Repository {
	t.Helper()
	requests := refundRepository.NewRefundRepository(h.db)
	h.service.WithRefunds(refundUsecase.NewService(
		uow.NewRunner(h.db),
		requests,
		h.orders,
		eventRepository.NewEventRepository(h.db),
		h.service,
		queueUsecase.NewDispatcher(nil),
	))
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM refund_requests WHERE event_id = ?", h.eventID)
	})
	return requests
}

func TestALatePaymentWithoutStockRefundsItself(t *testing.T) {
	h := newHarness(t, 2)
	requests := h.withRefunds(t)
	ctx := context.Background()

	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	h.expire(t, item)

	// Somebody else took the last two while this buyer was in their bank app.
	if reserved, err := h.tickets.Reserve(ctx, h.ticketID, 2); err != nil || !reserved {
		t.Fatalf("second buyer could not reserve: %t %v", reserved, err)
	}
	if err := h.tickets.Commit(ctx, h.ticketID, 2); err != nil {
		t.Fatalf("second buyer could not buy: %v", err)
	}

	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(ctx, paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	// The honest status, unchanged.
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefundRequired {
		t.Fatalf("status = %q, want %q", stored.Status, orderdomain.StatusRefundRequired)
	}

	// And now the part that did not exist: the refund itself.
	open, err := requests.FindOpenByOrder(ctx, item.ID)
	if err != nil {
		t.Fatalf("no refund was opened for an order that owes one: %v", err)
	}
	if open.Reason != refunddomain.ReasonOperator {
		t.Errorf("reason = %q, want %q: this was our timing, never the organiser's fault",
			open.Reason, refunddomain.ReasonOperator)
	}
	if open.Status != refunddomain.StatusApproved {
		t.Errorf("status = %q, want %q: nobody should have to approve a refund of money "+
			"we should not be holding", open.Status, refunddomain.StatusApproved)
	}
	if !open.AutoApproved() {
		t.Error("the refund names a decider; the policy decided this, not a person")
	}
	// The full amount, fee included. A buyer who never got in is not paying us
	// a service charge for the privilege.
	if open.AmountCents != item.TotalCents {
		t.Errorf("amount = %d, want the full %d the buyer paid", open.AmountCents, item.TotalCents)
	}

	// The money is actually scheduled, not merely recorded as owed.
	var jobs int64
	if err := h.db.Table("jobs").
		Where("type = ? AND dedupe_key = ?", queuedomain.TypeRefundCharge,
			string(queuedomain.TypeRefundCharge)+":"+item.ID).
		Count(&jobs).Error; err != nil {
		t.Fatalf("count refund jobs: %v", err)
	}
	if jobs != 1 {
		t.Errorf("%d refund job(s) queued, want exactly 1", jobs)
	}
}

// Providers redeliver, and a second delivery must not open a second refund:
// that would be two estornos for one payment.
func TestASecondDeliveryOpensOneRefund(t *testing.T) {
	h := newHarness(t, 2)
	h.withRefunds(t)
	ctx := context.Background()

	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	h.expire(t, item)
	if reserved, err := h.tickets.Reserve(ctx, h.ticketID, 2); err != nil || !reserved {
		t.Fatalf("second buyer could not reserve: %t %v", reserved, err)
	}
	if err := h.tickets.Commit(ctx, h.ticketID, 2); err != nil {
		t.Fatalf("second buyer could not buy: %v", err)
	}

	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	for attempt := 0; attempt < 3; attempt++ {
		if err := h.service.SyncPayment(ctx, paymentID); err != nil {
			t.Fatalf("SyncPayment() attempt %d error = %v", attempt, err)
		}
	}

	var opened int64
	if err := h.db.Table("refund_requests").Where("order_id = ?", item.ID).Count(&opened).Error; err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if opened != 1 {
		t.Errorf("%d refund requests for one order: a redelivered webhook opened a second estorno", opened)
	}
}

// Without the refund service the system must behave exactly as it did before:
// the order still tells the truth, and a person still has to act.
func TestWithoutTheRefundServiceSettlementIsUnchanged(t *testing.T) {
	h := newHarness(t, 2)
	ctx := context.Background()

	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	h.expire(t, item)
	if reserved, err := h.tickets.Reserve(ctx, h.ticketID, 2); err != nil || !reserved {
		t.Fatalf("second buyer could not reserve: %t %v", reserved, err)
	}
	if err := h.tickets.Commit(ctx, h.ticketID, 2); err != nil {
		t.Fatalf("second buyer could not buy: %v", err)
	}

	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(ctx, paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefundRequired {
		t.Fatalf("status = %q, want %q", stored.Status, orderdomain.StatusRefundRequired)
	}
}
