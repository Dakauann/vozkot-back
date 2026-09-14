package checkout

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	orderdomain "vozkot/domain/order"
	queuedomain "vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	orderRepository "vozkot/infra/repositories/order"
	queueRepository "vozkot/infra/repositories/queue"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
	"vozkot/infra/uow"
	queueUsecase "vozkot/usecases/queue"
)

// Everything here runs against a real PostgreSQL.
//
// It has to: the property under test is that two concurrent buyers cannot both
// take the last ticket, and that property lives in a conditional UPDATE and the
// row lock the database takes to evaluate it. A fake repository would be
// testing the fake's mutex.

type harness struct {
	db       *gorm.DB
	service  *Service
	orders   orderdomain.Repository
	tickets  ticketdomain.Repository
	jobs     queuedomain.Queue
	ticketID string
	ownerID  string
}

// newHarness builds a box office with no per-buyer hold cap, which is what most
// of these tests want: they are about inventory, and several of them make
// twenty orders for one account on purpose.
func newHarness(t *testing.T, capacity int) *harness {
	t.Helper()
	return newHarnessWithLimits(t, capacity, orderdomain.HoldLimits{})
}

func newHarnessWithLimits(t *testing.T, capacity int, limits orderdomain.HoldLimits) *harness {
	t.Helper()
	db := testsupport.Database(t)
	ctx := context.Background()

	tickets := ticketRepository.NewTicketRepository(db)
	orders := orderRepository.NewOrderRepository(db)
	jobs := queueRepository.NewJobRepository(db)

	ownerID := seedUser(t, db)
	item, err := ticketdomain.New(testsupport.Unique("tkt"), ownerID, ticketdomain.Draft{
		EventID:    testsupport.SeedEvent(t, db, ownerID),
		Title:      "Pista",
		PriceCents: 24000,
		Quantity:   capacity,
		Status:     ticketdomain.StatusOnSale,
	}, time.Now())
	if err != nil {
		t.Fatalf("build ticket: %v", err)
	}
	if err := tickets.Create(ctx, item); err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM jobs WHERE payload->>'orderId' IN (SELECT order_id FROM order_items WHERE ticket_id = ?)", item.ID)
		db.Exec("DELETE FROM orders WHERE id IN (SELECT order_id FROM order_items WHERE ticket_id = ?)", item.ID)
		db.Exec("DELETE FROM tickets WHERE id = ?", item.ID)
		db.Exec("DELETE FROM users WHERE id = ?", ownerID)
	})

	return &harness{
		db:       db,
		service:  NewService(uow.NewRunner(db), orders, tickets, queueUsecase.NewDispatcher(nil), 30*time.Minute, 10*time.Minute, limits),
		orders:   orders,
		tickets:  tickets,
		jobs:     jobs,
		ticketID: item.ID,
		ownerID:  ownerID,
	}
}

// seedUser creates the account a ticket's owner column points at.
func seedUser(t *testing.T, db *gorm.DB) string {
	t.Helper()
	id := testsupport.Unique("usr")
	err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Checkout Test', ?, 'x', 'user', 0, NOW(), NOW())`,
		id, id+"@vozkot.test").Error
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func (h *harness) start(quantity int, key string) (*orderdomain.Order, error) {
	return h.service.Start(context.Background(), StartInput{
		Items:          []orderdomain.DraftItem{{TicketID: h.ticketID, Quantity: quantity}},
		Confirm:        true,
		BuyerID:        h.ownerID,
		BuyerName:      "Maria Souza",
		BuyerEmail:     "maria@exemplo.com.br",
		BuyerDocument:  "12345678909",
		IdempotencyKey: key,
	})
}

func (h *harness) stock(t *testing.T) ticketdomain.Ticket {
	t.Helper()
	item, err := h.tickets.GetByID(context.Background(), h.ticketID)
	if err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	return *item
}

func TestStartHoldsStockAndSchedulesTheChargeInOneTransaction(t *testing.T) {
	h := newHarness(t, 10)

	item, err := h.start(2, testsupport.Unique("key"))

	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	stock := h.stock(t)
	if stock.Reserved != 2 || stock.Sold != 0 || stock.Available() != 8 {
		t.Fatalf("reserved=%d sold=%d available=%d, want 2/0/8", stock.Reserved, stock.Sold, stock.Available())
	}
	if item.Status != orderdomain.StatusPendingPayment {
		t.Fatalf("status = %q", item.Status)
	}

	// The outbox: both jobs were written by the same transaction that reserved
	// the stock, so work can never be scheduled for an order that rolled back.
	var jobTypes []string
	if err := h.db.Raw(
		"SELECT type FROM jobs WHERE payload->>'orderId' = ?", item.ID,
	).Scan(&jobTypes).Error; err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	found := map[string]bool{}
	for _, jobType := range jobTypes {
		found[jobType] = true
	}
	if !found[queuedomain.TypeCreateCharge] {
		t.Fatalf("jobs for the order = %v, want a charge job; the buyer would never get a PIX code", jobTypes)
	}
	if !found[queuedomain.TypeExpireHolds] {
		t.Fatalf("jobs for the order = %v, want an expiry job", jobTypes)
	}
}

func TestStartRollsBackTheHoldWhenTheOrderCannotBeWritten(t *testing.T) {
	h := newHarness(t, 5)
	key := testsupport.Unique("key")

	if _, err := h.start(2, key); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	// The same idempotency key twice: the unique index rejects the second
	// order, and the transaction must take its reservation down with it.
	_, err := h.start(2, key)

	if err == nil {
		t.Fatal("Start() accepted a duplicate idempotency key")
	}
	if stock := h.stock(t); stock.Reserved != 2 {
		t.Fatalf("reserved = %d, want 2: the failed attempt must hold nothing", stock.Reserved)
	}
}

func TestStartRefusesMoreThanIsAvailable(t *testing.T) {
	h := newHarness(t, 3)
	if _, err := h.start(2, testsupport.Unique("key")); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}

	_, err := h.start(2, testsupport.Unique("key"))

	if !errors.Is(err, ticketdomain.ErrInsufficientStock) {
		t.Fatalf("Start() error = %v, want %v", err, ticketdomain.ErrInsufficientStock)
	}
	if stock := h.stock(t); stock.Reserved != 2 {
		t.Fatalf("reserved = %d, want 2: a refused order must not hold anything", stock.Reserved)
	}
}

func TestStartRefusesTicketsThatAreNotOnSale(t *testing.T) {
	h := newHarness(t, 10)
	item, err := h.tickets.GetByID(context.Background(), h.ticketID)
	if err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	item.Status = ticketdomain.StatusDraft
	if err := h.tickets.Update(context.Background(), item); err != nil {
		t.Fatalf("update ticket: %v", err)
	}

	if _, err := h.start(1, testsupport.Unique("key")); !errors.Is(err, ticketdomain.ErrNotOnSale) {
		t.Fatalf("Start() error = %v, want %v", err, ticketdomain.ErrNotOnSale)
	}
}

// TestStartNeverOversells is the test the whole design exists to pass, and it
// only means anything against a real database.
//
// Twenty buyers race for eight tickets, two each. Exactly four can win. The
// losers must be refused rather than served a hold that pushes the tier past
// its capacity, and PostgreSQL's CHECK constraint would reject the row even if
// the application tried.
func TestStartNeverOversells(t *testing.T) {
	const capacity = 8
	const buyers = 20
	const each = 2

	h := newHarness(t, capacity)

	var wait sync.WaitGroup
	results := make([]error, buyers)
	start := make(chan struct{})
	wait.Add(buyers)
	for index := 0; index < buyers; index++ {
		go func(index int) {
			defer wait.Done()
			// Released together, so the requests actually contend.
			<-start
			_, err := h.start(each, testsupport.Unique("key"))
			results[index] = err
		}(index)
	}
	close(start)
	wait.Wait()

	succeeded := 0
	for index, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ticketdomain.ErrInsufficientStock):
		default:
			t.Fatalf("buyer %d failed unexpectedly: %v", index, err)
		}
	}

	if succeeded != capacity/each {
		t.Fatalf("%d buyers succeeded, want exactly %d", succeeded, capacity/each)
	}
	stock := h.stock(t)
	if stock.Reserved != capacity || stock.Available() != 0 {
		t.Fatalf("reserved = %d, available = %d, want %d and 0", stock.Reserved, stock.Available(), capacity)
	}
}

func TestCancelReturnsStockAndIsIdempotent(t *testing.T) {
	h := newHarness(t, 5)
	item, err := h.start(2, testsupport.Unique("key"))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	cancelled, err := h.service.Cancel(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if cancelled.Status != orderdomain.StatusCancelled {
		t.Fatalf("status = %q", cancelled.Status)
	}
	if stock := h.stock(t); stock.Reserved != 0 || stock.Available() != 5 {
		t.Fatalf("reserved = %d, available = %d, want 0 and 5", stock.Reserved, stock.Available())
	}

	// Tapping cancel twice is not a user error, and must not release stock
	// twice, which the database would refuse anyway.
	if _, err := h.service.Cancel(context.Background(), item.ID); err != nil {
		t.Fatalf("second Cancel() error = %v", err)
	}
	if stock := h.stock(t); stock.Reserved != 0 || stock.Available() != 5 {
		t.Fatalf("after a repeated cancel: reserved = %d, available = %d", stock.Reserved, stock.Available())
	}
}

func TestExpireHoldsClaimsEachLapsedOrderOnce(t *testing.T) {
	h := newHarness(t, 10)
	lapsed, err := h.start(3, testsupport.Unique("key"))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	fresh, err := h.start(2, testsupport.Unique("key"))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := h.db.Exec("UPDATE orders SET hold_expires_at = NOW() - INTERVAL '1 minute' WHERE id = ?", lapsed.ID).Error; err != nil {
		t.Fatalf("age the hold: %v", err)
	}

	// Two sweepers at once: with SKIP LOCKED they split the work, and the same
	// tickets are never released twice.
	var wait sync.WaitGroup
	released := make([]int, 2)
	errs := make([]error, 2)
	wait.Add(2)
	for index := 0; index < 2; index++ {
		go func(index int) {
			defer wait.Done()
			released[index], errs[index] = h.service.ExpireHolds(context.Background(), 10)
		}(index)
	}
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("sweeper %d error = %v", index, err)
		}
	}
	if released[0]+released[1] != 1 {
		t.Fatalf("sweepers released %d + %d, want exactly 1 between them", released[0], released[1])
	}
	if stock := h.stock(t); stock.Reserved != 2 {
		t.Fatalf("reserved = %d, want 2: only the lapsed hold comes back", stock.Reserved)
	}

	stillPending, err := h.orders.GetByID(context.Background(), fresh.ID)
	if err != nil || stillPending.Status != orderdomain.StatusPendingPayment {
		t.Fatalf("the unexpired order was disturbed: %+v (err %v)", stillPending, err)
	}
}

func TestExpireHoldIgnoresAnOrderPaidInTime(t *testing.T) {
	h := newHarness(t, 5)
	ctx := context.Background()
	item, err := h.start(2, testsupport.Unique("key"))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := item.Apply(orderdomain.StatusPaid, time.Now()); err != nil {
		t.Fatalf("Apply(paid) error = %v", err)
	}
	if err := h.orders.Update(ctx, item); err != nil {
		t.Fatalf("update order: %v", err)
	}
	for _, line := range item.Items {
		if err := h.tickets.Commit(ctx, line.TicketID, line.Quantity); err != nil {
			t.Fatalf("commit stock: %v", err)
		}
	}

	// The expiry job still fires: it was scheduled when the order was created.
	if err := h.service.ExpireHold(ctx, item.ID); err != nil {
		t.Fatalf("ExpireHold() error = %v", err)
	}

	stock := h.stock(t)
	if stock.Sold != 2 || stock.Reserved != 0 {
		t.Fatalf("sold = %d, reserved = %d, want 2 and 0: a paid order must not be expired", stock.Sold, stock.Reserved)
	}
}

func TestDatabaseRefusesStockThatWouldOversell(t *testing.T) {
	h := newHarness(t, 4)

	// The last line of defence, independent of every check in Go: even a direct
	// UPDATE cannot push sold plus reserved past the tier's capacity.
	err := h.db.Exec("UPDATE tickets SET reserved = 5 WHERE id = ?", h.ticketID).Error

	if err == nil {
		t.Fatal("the database accepted more held tickets than the tier has")
	}
}
