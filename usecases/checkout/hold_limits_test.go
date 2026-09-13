package checkout

import (
	"context"
	"errors"
	"sync"
	"testing"

	orderdomain "vozkot/domain/order"
	"vozkot/infra/testsupport"
)

// The hold cap is what closes the arithmetic the per-minute rate limit leaves
// open.
//
// Thirty checkouts a minute, ten tickets each and a thirty-minute hold means one
// account can keep nine thousand tickets off the shelf indefinitely by cycling,
// with no money at risk and no limit ever tripped — the rate limit bounds how
// FAST someone reserves, never how MUCH they are sitting on, and it is the
// second number that empties an event.
//
// All of this runs against a real PostgreSQL, because the property under test
// is that the count and the insert it guards are one indivisible step. That
// lives in a transaction-scoped advisory lock, and a fake would be testing the
// fake's mutex.

func TestOpenOrderLimitRefusesTheNextCheckout(t *testing.T) {
	h := newHarnessWithLimits(t, 100, orderdomain.HoldLimits{Orders: 2})

	if _, err := h.start(1, testsupport.Unique("key")); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if _, err := h.start(1, testsupport.Unique("key")); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}

	_, err := h.start(1, testsupport.Unique("key"))

	if !errors.Is(err, orderdomain.ErrTooManyOpenOrders) {
		t.Fatalf("third Start() error = %v, want %v", err, orderdomain.ErrTooManyOpenOrders)
	}
	if stock := h.stock(t); stock.Reserved != 2 {
		t.Fatalf("reserved = %d, want 2: a refused checkout must hold nothing", stock.Reserved)
	}
}

func TestPerTierLimitCountsTheOrderBeingPlaced(t *testing.T) {
	// Six held already, asking for five more: the question is what the buyer
	// would hold AFTER this checkout, not before it.
	h := newHarnessWithLimits(t, 100, orderdomain.HoldLimits{TicketsPerTier: 10})

	if _, err := h.start(6, testsupport.Unique("key")); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}

	if _, err := h.start(5, testsupport.Unique("key")); !errors.Is(err, orderdomain.ErrTooManyHeldTickets) {
		t.Fatalf("Start(5) error = %v, want %v", err, orderdomain.ErrTooManyHeldTickets)
	}
	// Four exactly reaches the ceiling and must be allowed: a limit that
	// refuses the last legitimate ticket is a bug, not a safety margin.
	if _, err := h.start(4, testsupport.Unique("key")); err != nil {
		t.Fatalf("Start(4) error = %v, want the tenth ticket to be allowed", err)
	}
	if stock := h.stock(t); stock.Reserved != 10 {
		t.Fatalf("reserved = %d, want 10", stock.Reserved)
	}
}

// TestHoldLimitSurvivesConcurrentCheckouts is the test the advisory lock exists
// to pass.
//
// Without it, eight simultaneous checkouts by one account all read "zero open
// orders", all pass a limit of two, and all commit — a limit that holds only
// when nobody tries, which is the same as no limit at all against someone
// actually trying.
func TestHoldLimitSurvivesConcurrentCheckouts(t *testing.T) {
	const limit = 2
	const attempts = 8

	h := newHarnessWithLimits(t, 100, orderdomain.HoldLimits{Orders: limit})

	var wait sync.WaitGroup
	results := make([]error, attempts)
	release := make(chan struct{})
	wait.Add(attempts)
	for index := 0; index < attempts; index++ {
		go func(index int) {
			defer wait.Done()
			<-release
			_, results[index] = h.start(1, testsupport.Unique("key"))
		}(index)
	}
	close(release)
	wait.Wait()

	succeeded := 0
	for index, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, orderdomain.ErrTooManyOpenOrders):
		default:
			t.Fatalf("attempt %d failed unexpectedly: %v", index, err)
		}
	}

	if succeeded != limit {
		t.Fatalf("%d of %d concurrent checkouts succeeded, want exactly %d", succeeded, attempts, limit)
	}
	if stock := h.stock(t); stock.Reserved != limit {
		t.Fatalf("reserved = %d, want %d: the cap must hold under contention or it holds nothing", stock.Reserved, limit)
	}
}

// TestCancellingFreesTheAllowance: the cap is on OPEN holds, so a buyer who
// changes their mind is not locked out of the box office for half an hour.
func TestCancellingFreesTheAllowance(t *testing.T) {
	h := newHarnessWithLimits(t, 100, orderdomain.HoldLimits{Orders: 1})
	ctx := context.Background()

	first, err := h.start(1, testsupport.Unique("key"))
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if _, err := h.start(1, testsupport.Unique("key")); !errors.Is(err, orderdomain.ErrTooManyOpenOrders) {
		t.Fatalf("second Start() error = %v, want the cap to bite", err)
	}

	if _, err := h.service.Cancel(ctx, first.ID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}

	if _, err := h.start(1, testsupport.Unique("key")); err != nil {
		t.Fatalf("Start() after a cancel error = %v: only OPEN holds may count", err)
	}
}

// TestExpiredHoldsFreeTheAllowance: the same, for the buyer who simply never
// paid. Their allowance returns with the stock.
func TestExpiredHoldsFreeTheAllowance(t *testing.T) {
	h := newHarnessWithLimits(t, 100, orderdomain.HoldLimits{Orders: 1})
	ctx := context.Background()

	first, err := h.start(1, testsupport.Unique("key"))
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if err := h.db.Exec("UPDATE orders SET hold_expires_at = NOW() - INTERVAL '1 minute' WHERE id = ?", first.ID).Error; err != nil {
		t.Fatalf("age the hold: %v", err)
	}
	if _, err := h.service.ExpireHolds(ctx, 10); err != nil {
		t.Fatalf("ExpireHolds() error = %v", err)
	}

	if _, err := h.start(1, testsupport.Unique("key")); err != nil {
		t.Fatalf("Start() after an expiry error = %v", err)
	}
}

// TestOneBuyersLimitDoesNotBlockAnother: the lock is per account, so the cap
// costs an honest buyer nothing. It is only ever contended by someone checking
// out twice at once.
func TestOneBuyersLimitDoesNotBlockAnother(t *testing.T) {
	h := newHarnessWithLimits(t, 100, orderdomain.HoldLimits{Orders: 1})
	other := seedUser(t, h.db)
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM jobs WHERE payload->>'orderId' IN (SELECT id FROM orders WHERE buyer_id = ?)", other)
		h.db.Exec("DELETE FROM orders WHERE buyer_id = ?", other)
		h.db.Exec("DELETE FROM users WHERE id = ?", other)
	})

	if _, err := h.start(1, testsupport.Unique("key")); err != nil {
		t.Fatalf("first buyer Start() error = %v", err)
	}

	_, err := h.service.Start(context.Background(), StartInput{
		Items:          []orderdomain.DraftItem{{TicketID: h.ticketID, Quantity: 1}},
		Confirm:        true,
		BuyerID:        other,
		BuyerName:      "Joao Lima",
		BuyerEmail:     "joao@exemplo.com.br",
		BuyerDocument:  "12345678909",
		IdempotencyKey: testsupport.Unique("key"),
	})

	if err != nil {
		t.Fatalf("a second buyer was refused because of the first buyer's holds: %v", err)
	}
	if stock := h.stock(t); stock.Reserved != 2 {
		t.Fatalf("reserved = %d, want 2", stock.Reserved)
	}
}

// TestUnlimitedHoldsSkipTheLockEntirely: the zero value caps nothing, which is
// what the load harness and a box office selling at the door both want.
func TestUnlimitedHoldsSkipTheLockEntirely(t *testing.T) {
	h := newHarnessWithLimits(t, 100, orderdomain.HoldLimits{})

	for index := 0; index < 5; index++ {
		if _, err := h.start(1, testsupport.Unique("key")); err != nil {
			t.Fatalf("Start() %d error = %v, want no cap", index, err)
		}
	}
	if stock := h.stock(t); stock.Reserved != 5 {
		t.Fatalf("reserved = %d, want 5", stock.Reserved)
	}
}
