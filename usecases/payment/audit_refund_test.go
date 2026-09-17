package payment

import (
	"context"
	"errors"
	"testing"

	authdomain "vozkot/domain/auth"
	orderdomain "vozkot/domain/order"
	queuedomain "vozkot/domain/queue"
	userdomain "vozkot/domain/user"
	"vozkot/infra/mercadopago"
)

// Two holes this file closes, both on the money side.
//
// Reconciliation only ever looks at orders still WAITING for money, so a refund
// or a chargeback raised in the provider's own dashboard reaches the box office
// through exactly one notification. Lose it and the order stays paid and the
// seat stays sold for good: the money went back, the inventory never did, and
// nothing in the system would ever notice.
//
// And a refund used to run on the HTTP request. A provider slower than the
// twenty-second write timeout left the money refunded at Mercado Pago and the
// order untouched here; the two facts that must never disagree, disagreeing,
// with nothing left to reconcile them.

// paid drives an order all the way to paid, the way a webhook would.
func (h *harness) paid(t *testing.T, quantity int) *orderdomain.Order {
	t.Helper()
	item := h.pendingOrder(t, quantity)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusPaid {
		t.Fatalf("order status = %q, want paid", stored.Status)
	}
	return h.order(t, item.ID)
}

func (h *harness) openJobs(t *testing.T, jobType, orderID string) int64 {
	t.Helper()
	var total int64
	err := h.db.Raw(`
		SELECT COUNT(*) FROM jobs
		WHERE type = ? AND payload->>'orderId' = ? AND status IN ('pending','processing')`,
		jobType, orderID).Scan(&total).Error
	if err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return total
}

// TestAuditSchedulesARereadOfSettledOrders is the sweep that notices a refund
// nobody told us about.
func TestAuditSchedulesARereadOfSettledOrders(t *testing.T) {
	h := newHarness(t, 20)
	item := h.paid(t, 2)

	scheduled, err := h.service.AuditSettled(context.Background(), 50)

	if err != nil {
		t.Fatalf("AuditSettled() error = %v", err)
	}
	if scheduled < 1 {
		t.Fatal("the audit scheduled nothing for a paid order whose event is still ahead")
	}
	if got := h.openJobs(t, queuedomain.TypeSyncPayment, item.ID); got != 1 {
		t.Fatalf("open sync jobs = %d, want 1", got)
	}
}

// TestAuditRecoversARefundNobodyWasToldAbout is the whole point, end to end:
// the provider's state moves, no notification arrives, and the audit is the
// only thing that puts the seat back on sale.
func TestAuditRecoversARefundNobodyWasToldAbout(t *testing.T) {
	h := newHarness(t, 20)
	item := h.paid(t, 2)
	if stock := h.stock(t); stock.Sold != 2 {
		t.Fatalf("sold = %d, want 2 before the refund", stock.Sold)
	}

	// Refunded in the Mercado Pago dashboard. No webhook is delivered.
	h.provider.move(item.PaymentID, mercadopago.StatusRefunded, "")

	if _, err := h.service.AuditSettled(context.Background(), 50); err != nil {
		t.Fatalf("AuditSettled() error = %v", err)
	}
	// The audit schedules the read; running it is what settles.
	if err := h.service.SyncPayment(context.Background(), item.PaymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefunded {
		t.Fatalf("order status = %q, want refunded", stored.Status)
	}
	if stock := h.stock(t); stock.Sold != 0 {
		t.Fatalf("sold = %d, want 0: the money went back and the seat did not", stock.Sold)
	}
}

// TestAuditIgnoresOrdersWhoseEventHasPassed bounds the work. Re-reading every
// order ever paid, forever, is unbounded provider traffic for no benefit: once
// the doors have closed a late refund is bookkeeping, not a ticket somebody
// else could have bought.
func TestAuditIgnoresOrdersWhoseEventHasPassed(t *testing.T) {
	h := newHarness(t, 20)
	item := h.paid(t, 1)
	// The date lives on the EVENT now, not on the tier whose price was paid.
	err := h.db.Exec(`
		UPDATE events SET starts_at = NOW() - INTERVAL '1 day'
		WHERE id = (SELECT event_id FROM tickets WHERE id = ?)`, h.ticketID).Error
	if err != nil {
		t.Fatalf("age the event: %v", err)
	}

	scheduled, auditErr := h.service.AuditSettled(context.Background(), 50)

	if auditErr != nil {
		t.Fatalf("AuditSettled() error = %v", auditErr)
	}
	if got := h.openJobs(t, queuedomain.TypeSyncPayment, item.ID); got != 0 {
		t.Fatalf("the audit scheduled %d job(s) for an event that already happened (scheduled=%d)", got, scheduled)
	}
}

// TestAuditSharesTheDedupeKeyWithEveryOtherPath: an audit racing a webhook is
// the same work, not a second provider call.
func TestAuditSharesTheDedupeKeyWithEveryOtherPath(t *testing.T) {
	h := newHarness(t, 20)
	item := h.paid(t, 1)

	for round := 0; round < 3; round++ {
		if _, err := h.service.AuditSettled(context.Background(), 50); err != nil {
			t.Fatalf("AuditSettled() round %d error = %v", round, err)
		}
	}

	if got := h.openJobs(t, queuedomain.TypeSyncPayment, item.ID); got != 1 {
		t.Fatalf("open sync jobs = %d after three audits, want 1", got)
	}
}

// TestRequestRefundSchedulesInsteadOfCalling: the provider is not touched on
// the request, so a slow provider cannot strand the money.
func TestRequestRefundSchedulesInsteadOfCalling(t *testing.T) {
	h := newHarness(t, 20)
	item := h.paid(t, 2)

	returned, err := h.service.RequestRefund(context.Background(), authdomain.Actor{ID: "usr_ops", Role: userdomain.RoleAdmin}, item.ID)

	if err != nil {
		t.Fatalf("RequestRefund() error = %v", err)
	}
	if returned.Status != orderdomain.StatusPaid {
		t.Fatalf("returned status = %q, want the order as it stands", returned.Status)
	}
	if got := h.openJobs(t, queuedomain.TypeRefundCharge, item.ID); got != 1 {
		t.Fatalf("open refund jobs = %d, want 1", got)
	}
	// Nothing has moved yet: the job has not run.
	if stock := h.stock(t); stock.Sold != 2 {
		t.Fatalf("sold = %d, want 2 until the refund job runs", stock.Sold)
	}
}

// TestPressingRefundTwiceRefundsOnce: an operator double-clicking must not
// send two refunds.
func TestPressingRefundTwiceRefundsOnce(t *testing.T) {
	h := newHarness(t, 20)
	item := h.paid(t, 1)

	for round := 0; round < 3; round++ {
		if _, err := h.service.RequestRefund(context.Background(), authdomain.Actor{ID: "usr_ops", Role: userdomain.RoleAdmin}, item.ID); err != nil {
			t.Fatalf("RequestRefund() round %d error = %v", round, err)
		}
	}

	if got := h.openJobs(t, queuedomain.TypeRefundCharge, item.ID); got != 1 {
		t.Fatalf("open refund jobs = %d, want 1", got)
	}
}

// TestTheRefundJobGivesTheMoneyAndTheSeatBack: the handler behind the job,
// running the real provider call and the read-back that confirms it.
func TestTheRefundJobGivesTheMoneyAndTheSeatBack(t *testing.T) {
	h := newHarness(t, 20)
	item := h.paid(t, 2)

	if err := h.service.Refund(context.Background(), item.ID); err != nil {
		t.Fatalf("Refund() error = %v", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefunded {
		t.Fatalf("order status = %q, want refunded", stored.Status)
	}
	if stock := h.stock(t); stock.Sold != 0 {
		t.Fatalf("sold = %d, want 0: a refunded ticket goes back on sale", stock.Sold)
	}
}

// TestTheRefundJobIsSafeToRunTwice: the queue is at-least-once, so the handler
// has to be.
func TestTheRefundJobIsSafeToRunTwice(t *testing.T) {
	h := newHarness(t, 20)
	item := h.paid(t, 2)
	if err := h.service.Refund(context.Background(), item.ID); err != nil {
		t.Fatalf("first Refund() error = %v", err)
	}

	if err := h.service.Refund(context.Background(), item.ID); err != nil {
		t.Fatalf("second Refund() error = %v: a redelivered job must be a no-op", err)
	}

	if stock := h.stock(t); stock.Sold != 0 {
		t.Fatalf("sold = %d, want 0: the second run moved stock again", stock.Sold)
	}
}

// TestRefundingAnUnpaidOrderIsRefusedImmediately: the caller learns now, rather
// than through a job that parks and waits for somebody to read the log.
func TestRefundingAnUnpaidOrderIsRefusedImmediately(t *testing.T) {
	h := newHarness(t, 20)
	item := h.pendingOrder(t, 1)

	_, err := h.service.RequestRefund(context.Background(), authdomain.Actor{ID: "usr_ops", Role: userdomain.RoleAdmin}, item.ID)

	if !errors.Is(err, ErrNotRefundable) {
		t.Fatalf("RequestRefund() error = %v, want %v", err, ErrNotRefundable)
	}
	if got := h.openJobs(t, queuedomain.TypeRefundCharge, item.ID); got != 0 {
		t.Fatalf("a refund job was scheduled for an order that was never charged")
	}
}

func TestRefundingAnUnknownOrderIsRefusedImmediately(t *testing.T) {
	h := newHarness(t, 20)

	_, err := h.service.RequestRefund(context.Background(), authdomain.Actor{ID: "usr_ops", Role: userdomain.RoleAdmin}, "ord_does_not_exist")

	if !errors.Is(err, orderdomain.ErrNotFound) {
		t.Fatalf("RequestRefund() error = %v, want %v", err, orderdomain.ErrNotFound)
	}
}

// TestTheRefundJobParksOnAFailureNoRetryCanFix: an order that vanished is not
// going to reappear on attempt eight, and a job that needs a person belongs
// where a person will see it rather than hidden in the retry queue.
func TestTheRefundJobParksOnAFailureNoRetryCanFix(t *testing.T) {
	h := newHarness(t, 20)

	err := h.service.Refund(context.Background(), "ord_does_not_exist")

	if !queuedomain.IsPermanent(err) {
		t.Fatalf("Refund() error = %v, want a permanent failure so the job parks on the first attempt", err)
	}
}

// TestARefundRequiredOrderCanStillBeRefunded: money taken for tickets that were
// already resold is precisely the case an operator needs the button for.
func TestARefundRequiredOrderCanStillBeRefunded(t *testing.T) {
	h := newHarness(t, 20)
	item := h.paid(t, 1)
	if err := h.db.Exec("UPDATE orders SET status = ? WHERE id = ?",
		string(orderdomain.StatusRefundRequired), item.ID).Error; err != nil {
		t.Fatalf("set refund_required: %v", err)
	}

	if _, err := h.service.RequestRefund(context.Background(), authdomain.Actor{ID: "usr_ops", Role: userdomain.RoleAdmin}, item.ID); err != nil {
		t.Fatalf("RequestRefund() error = %v, want an order owing a refund to be refundable", err)
	}
	if got := h.openJobs(t, queuedomain.TypeRefundCharge, item.ID); got != 1 {
		t.Fatalf("open refund jobs = %d, want 1", got)
	}
}

// TestTheAuditDoesNotDisturbPendingOrders: the two sweeps have different jobs
// and must not step on one another.
func TestTheAuditDoesNotDisturbPendingOrders(t *testing.T) {
	h := newHarness(t, 20)
	pending := h.pendingOrder(t, 1)
	before := h.order(t, pending.ID).UpdatedAt

	if _, err := h.service.AuditSettled(context.Background(), 50); err != nil {
		t.Fatalf("AuditSettled() error = %v", err)
	}

	after := h.order(t, pending.ID)
	if after.Status != orderdomain.StatusPendingPayment {
		t.Fatalf("a pending order became %q during a settled-order audit", after.Status)
	}
	if !after.UpdatedAt.Equal(before) {
		t.Fatal("the audit touched an order that is still waiting for payment")
	}
}
