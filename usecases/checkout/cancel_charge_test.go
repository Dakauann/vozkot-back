package checkout

import (
	"context"
	"testing"

	"vozkot/infra/uow"
	paymentUsecase "vozkot/usecases/payment"
	queueUsecase "vozkot/usecases/queue"

	queuedomain "vozkot/domain/queue"
)

// An expiring hold takes its payment code with it.
//
// THE WINDOW THIS CLOSES: a hold is thirty minutes and a provider dates a
// charge to a DAY, so releasing an order's stock left a live, payable PIX code
// out in the world for another twenty-odd hours. Whoever paid it sent real
// money for seats already back on sale, and the box office could only take it
// and give it straight back, minus the provider's fee.
//
// The voider is the REAL payment service, built with a nil gateway: enqueueing
// the void writes a job row and touches no provider, so nothing here is
// stubbed, and the job this asserts is the one production writes.
func (b *basket) withChargeVoider() {
	b.service.WithChargeVoider(paymentUsecase.NewService(
		uow.NewRunner(b.db), b.orders, nil, b.jobs, queueUsecase.NewDispatcher(nil), nil,
	))
}

// charged gives the order a payment id, which is what an issued PIX looks like
// from this side.
func (b *basket) charged(t *testing.T, orderID string) {
	t.Helper()
	err := b.db.Exec("UPDATE orders SET payment_id = ?, payment_provider = 'mercadopago' WHERE id = ?",
		"pay_"+orderID, orderID).Error
	if err != nil {
		t.Fatalf("attach a payment id: %v", err)
	}
}

func (b *basket) hasJob(t *testing.T, orderID, jobType string) bool {
	t.Helper()
	for _, found := range b.jobTypes(t, orderID) {
		if found == jobType {
			return true
		}
	}
	return false
}

func TestExpiringOneHoldVoidsItsCharge(t *testing.T) {
	b := newBasket(t, 10, 10)
	b.withChargeVoider()

	item, err := b.reserve("", line(b.pista, 2))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	b.charged(t, item.ID)
	if err := b.db.Exec("UPDATE orders SET hold_expires_at = NOW() - INTERVAL '1 minute' WHERE id = ?", item.ID).Error; err != nil {
		t.Fatalf("age the hold: %v", err)
	}

	if err := b.service.ExpireHold(context.Background(), item.ID); err != nil {
		t.Fatalf("ExpireHold() error = %v", err)
	}

	if got := b.stock(t, b.pista).Reserved; got != 0 {
		t.Fatalf("pista reserved = %d, want 0", got)
	}
	if !b.hasJob(t, item.ID, queuedomain.TypeCancelCharge) {
		t.Error("the stock came back but the charge did not: the PIX code stays payable for " +
			"seats that are on sale again, and whoever pays it is owed a refund")
	}
}

// The sweep, which is the path that handles an abandoned cart nobody scheduled
// an expiry job for.
func TestTheSweepVoidsTheChargesItReleases(t *testing.T) {
	b := newBasket(t, 10, 10)
	b.withChargeVoider()

	item, err := b.reserve("", line(b.pista, 1))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	b.charged(t, item.ID)
	if err := b.db.Exec("UPDATE orders SET hold_expires_at = NOW() - INTERVAL '1 minute' WHERE id = ?", item.ID).Error; err != nil {
		t.Fatalf("age the hold: %v", err)
	}

	if _, err := b.service.ExpireHolds(context.Background(), 10); err != nil {
		t.Fatalf("ExpireHolds() error = %v", err)
	}
	if !b.hasJob(t, item.ID, queuedomain.TypeCancelCharge) {
		t.Error("the sweep released the stock without voiding the charge")
	}
}

// Most abandoned carts never reach a provider: the hold lapses before anybody
// confirms. Queueing a void for a charge that was never issued would be a job
// per abandoned cart, all of them finding nothing.
func TestAnUnchargedHoldQueuesNoVoid(t *testing.T) {
	b := newBasket(t, 10, 10)
	b.withChargeVoider()

	item, err := b.reserve("", line(b.pista, 1))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	if err := b.db.Exec("UPDATE orders SET hold_expires_at = NOW() - INTERVAL '1 minute' WHERE id = ?", item.ID).Error; err != nil {
		t.Fatalf("age the hold: %v", err)
	}

	if err := b.service.ExpireHold(context.Background(), item.ID); err != nil {
		t.Fatalf("ExpireHold() error = %v", err)
	}
	if b.hasJob(t, item.ID, queuedomain.TypeCancelCharge) {
		t.Error("a void was queued for an order that was never charged")
	}
}

// Without the voider, expiry behaves exactly as it did before.
func TestExpiryWithoutAVoiderIsUnchanged(t *testing.T) {
	b := newBasket(t, 10, 10)

	item, err := b.reserve("", line(b.pista, 2))
	if err != nil {
		t.Fatalf("reserve() error = %v", err)
	}
	b.charged(t, item.ID)
	if err := b.db.Exec("UPDATE orders SET hold_expires_at = NOW() - INTERVAL '1 minute' WHERE id = ?", item.ID).Error; err != nil {
		t.Fatalf("age the hold: %v", err)
	}

	if err := b.service.ExpireHold(context.Background(), item.ID); err != nil {
		t.Fatalf("ExpireHold() error = %v", err)
	}
	if got := b.stock(t, b.pista).Reserved; got != 0 {
		t.Fatalf("pista reserved = %d, want 0", got)
	}
}
