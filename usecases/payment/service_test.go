package payment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	notificationdomain "vozkot/domain/notification"
	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	queuedomain "vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/infra/mercadopago"
	orderRepository "vozkot/infra/repositories/order"
	queueRepository "vozkot/infra/repositories/queue"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
	"vozkot/infra/uow"
	notificationUsecase "vozkot/usecases/notification"
	queueUsecase "vozkot/usecases/queue"
)

// The provider is stubbed at the HTTP boundary and nowhere else: requests go
// through the real Mercado Pago client and adapter, so what is under test is
// the settlement rules AND the wire format together. The stub is stateful — a
// payment it created can later be moved to approved, rejected or refunded — so
// each test drives the same sequence a real webhook-and-fetch would.

type stubProvider struct {
	mu       sync.Mutex
	payments map[string]map[string]any
	created  int
	// bornAs is the status a new payment starts in.
	bornAs string
	// failCreate makes POST /v1/payments answer with this status code.
	failCreate int
	nextID     int64
	// duringCreate runs while POST /v1/payments is still in flight, which is
	// how a test makes something else happen to the order at the one moment
	// the charge job is waiting on the provider. It must not call back into
	// the stub: the handler's lock is held.
	duringCreate func()
}

func newStubProvider() *stubProvider {
	return &stubProvider{payments: map[string]map[string]any{}, bornAs: mercadopago.StatusPending, nextID: 9000000000}
}

func (p *stubProvider) handler() http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		response.Header().Set("Content-Type", "application/json")

		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/payments":
			if p.failCreate != 0 {
				response.WriteHeader(p.failCreate)
				_, _ = response.Write([]byte(`{"message":"provider unavailable"}`))
				return
			}
			var body mercadopago.CreatePaymentRequest
			_ = json.NewDecoder(request.Body).Decode(&body)
			p.created++
			p.nextID++
			id := strconv.FormatInt(p.nextID, 10)
			payment := map[string]any{
				"id":                 p.nextID,
				"status":             p.bornAs,
				"status_detail":      "",
				"external_reference": body.ExternalReference,
				"payment_method_id":  "pix",
				"transaction_amount": body.TransactionAmount,
				"point_of_interaction": map[string]any{"transaction_data": map[string]any{
					"qr_code":        "00020126-" + id,
					"qr_code_base64": "aGVsbG8=",
				}},
			}
			p.payments[id] = payment
			if p.duringCreate != nil {
				p.duringCreate()
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(payment)

		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/payments/"):
			id := strings.TrimPrefix(request.URL.Path, "/v1/payments/")
			payment, ok := p.payments[id]
			if !ok {
				response.WriteHeader(http.StatusNotFound)
				_, _ = response.Write([]byte(`{"message":"Payment not found","status":404}`))
				return
			}
			_ = json.NewEncoder(response).Encode(payment)

		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/refunds"):
			id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/v1/payments/"), "/refunds")
			if payment, ok := p.payments[id]; ok {
				payment["status"] = mercadopago.StatusRefunded
				payment["transaction_amount_refunded"] = payment["transaction_amount"]
			}
			_, _ = response.Write([]byte(`{"id":1,"status":"approved"}`))

		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}
}

// move sets the state the provider reports for a payment from now on.
func (p *stubProvider) move(id, status string, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if payment, ok := p.payments[id]; ok {
		payment["status"] = status
		payment["status_detail"] = detail
	}
}

type harness struct {
	db       *gorm.DB
	service  *Service
	orders   orderdomain.Repository
	tickets  ticketdomain.Repository
	provider *stubProvider
	ticketID string
	ownerID  string
}

func newHarness(t *testing.T, capacity int) *harness {
	t.Helper()
	db := testsupport.Database(t)
	ctx := context.Background()

	provider := newStubProvider()
	server := httptest.NewServer(provider.handler())
	t.Cleanup(server.Close)
	client := mercadopago.NewClient("TEST-token", server.URL, mercadopago.WithNotificationURL("https://api.test/webhooks/mercadopago"))
	gateway := mercadopago.NewGateway(client)

	tickets := ticketRepository.NewTicketRepository(db)
	orders := orderRepository.NewOrderRepository(db)
	jobs := queueRepository.NewJobRepository(db)

	ownerID := testsupport.Unique("usr")
	if err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Payment Test', ?, 'x', 'user', 0, NOW(), NOW())`, ownerID, ownerID+"@vozkot.test").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	ticket, err := ticketdomain.New(testsupport.Unique("tkt"), ownerID, ticketdomain.Draft{
		EventName:  "Festival Aurora",
		Title:      "Pista",
		Venue:      "Arena",
		StartsAt:   time.Now().Add(720 * time.Hour),
		PriceCents: 24000,
		Quantity:   capacity,
		Status:     ticketdomain.StatusOnSale,
	}, time.Now())
	if err != nil {
		t.Fatalf("build ticket: %v", err)
	}
	if err := tickets.Create(ctx, ticket); err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	t.Cleanup(func() {
		// Notification jobs first: they are found through the orders, which the
		// next statement removes.
		db.Exec(`DELETE FROM jobs WHERE type = ? AND EXISTS (
			SELECT 1 FROM orders o WHERE o.ticket_id = ? AND jobs.dedupe_key LIKE '%:' || o.id)`,
			queuedomain.TypeSendNotification, ticket.ID)
		db.Exec("DELETE FROM jobs WHERE payload->>'orderId' IN (SELECT id FROM orders WHERE ticket_id = ?)", ticket.ID)
		db.Exec("DELETE FROM orders WHERE ticket_id = ?", ticket.ID)
		db.Exec("DELETE FROM tickets WHERE id = ?", ticket.ID)
		db.Exec("DELETE FROM users WHERE id = ?", ownerID)
	})

	// A real notifier over the real job repository. Nothing is stubbed: what
	// the test asserts is the row settlement actually commits.
	notifier := notificationUsecase.NewNotifier(jobs, queueUsecase.NewDispatcher(nil), notificationdomain.ChannelEmail)
	purchases := notificationUsecase.NewPurchases(notifier, "https://tickets.test")

	return &harness{
		db:       db,
		service:  NewService(uow.NewRunner(db), orders, gateway, jobs, queueUsecase.NewDispatcher(nil), purchases),
		orders:   orders,
		tickets:  tickets,
		provider: provider,
		ticketID: ticket.ID,
		ownerID:  ownerID,
	}
}

// pendingOrder creates a held order the way checkout would.
func (h *harness) pendingOrder(t *testing.T, quantity int) *orderdomain.Order {
	t.Helper()
	ctx := context.Background()

	reserved, err := h.tickets.Reserve(ctx, h.ticketID, quantity)
	if err != nil || !reserved {
		t.Fatalf("reserve: reserved=%t err=%v", reserved, err)
	}
	item, err := orderdomain.New(testsupport.Unique("ord"), orderdomain.Draft{
		TicketID:       h.ticketID,
		BuyerID:        h.ownerID,
		BuyerName:      "Maria Souza",
		BuyerEmail:     "maria@exemplo.com.br",
		BuyerDocument:  "12345678909",
		Quantity:       quantity,
		UnitPriceCents: 24000,
	}, 30*time.Minute, time.Now())
	if err != nil {
		t.Fatalf("build order: %v", err)
	}
	if err := h.orders.Create(ctx, item); err != nil {
		t.Fatalf("create order: %v", err)
	}
	return item
}

func (h *harness) charge(t *testing.T, item *orderdomain.Order) string {
	t.Helper()
	if err := h.service.CreateCharge(context.Background(), item.ID); err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	return h.order(t, item.ID).PaymentID
}

func (h *harness) stock(t *testing.T) ticketdomain.Ticket {
	t.Helper()
	item, err := h.tickets.GetByID(context.Background(), h.ticketID)
	if err != nil {
		t.Fatalf("read ticket: %v", err)
	}
	return *item
}

func (h *harness) order(t *testing.T, id string) *orderdomain.Order {
	t.Helper()
	item, err := h.orders.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	return item
}

// expire mimics the sweep: the order expires and its stock goes back.
func (h *harness) expire(t *testing.T, item *orderdomain.Order) {
	t.Helper()
	if err := h.expireOrder(item.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
}

// expireOrder is expire without a *testing.T, so it can be called from the
// provider stub's goroutine — where t.Fatalf would be illegal.
func (h *harness) expireOrder(orderID string) error {
	ctx := context.Background()
	stored, err := h.orders.GetByID(ctx, orderID)
	if err != nil {
		return err
	}
	if _, err := stored.Apply(orderdomain.StatusExpired, time.Now()); err != nil {
		return err
	}
	if err := h.orders.Update(ctx, stored); err != nil {
		return err
	}
	return h.tickets.Release(ctx, stored.TicketID, stored.Quantity)
}

func TestCreateChargeAttachesThePixCodeWithoutMovingTheOrder(t *testing.T) {
	h := newHarness(t, 10)
	item := h.pendingOrder(t, 2)

	paymentID := h.charge(t, item)

	stored := h.order(t, item.ID)
	if paymentID == "" || stored.PixCopyPaste == "" {
		t.Fatalf("charge details missing: paymentID=%q pix=%q", paymentID, stored.PixCopyPaste)
	}
	if stored.Status != orderdomain.StatusPendingPayment {
		t.Fatalf("status = %q; a created charge is still unpaid", stored.Status)
	}
	if stock := h.stock(t); stock.Reserved != 2 || stock.Sold != 0 {
		t.Fatalf("stock moved on charge creation: reserved=%d sold=%d", stock.Reserved, stock.Sold)
	}
}

func TestCreateChargeNeverChargesTwice(t *testing.T) {
	h := newHarness(t, 10)
	item := h.pendingOrder(t, 1)

	for i := 0; i < 3; i++ {
		if err := h.service.CreateCharge(context.Background(), item.ID); err != nil {
			t.Fatalf("CreateCharge() attempt %d error = %v", i+1, err)
		}
	}

	if h.provider.created != 1 {
		t.Fatalf("provider created %d charges, want 1: a retried job must not charge again", h.provider.created)
	}
}

func TestCreateChargeSkipsOrdersThatAreNoLongerPending(t *testing.T) {
	h := newHarness(t, 10)
	item := h.pendingOrder(t, 1)
	if _, err := item.Apply(orderdomain.StatusCancelled, time.Now()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := h.orders.Update(context.Background(), item); err != nil {
		t.Fatalf("update order: %v", err)
	}

	if err := h.service.CreateCharge(context.Background(), item.ID); err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	if h.provider.created != 0 {
		t.Fatal("a cancelled order was charged")
	}
}

func TestCreateChargeSurfacesProviderFailuresForRetry(t *testing.T) {
	h := newHarness(t, 5)
	item := h.pendingOrder(t, 1)
	h.provider.failCreate = http.StatusBadGateway

	err := h.service.CreateCharge(context.Background(), item.ID)

	if err == nil {
		t.Fatal("CreateCharge() returned nil; the job would complete and the buyer be left without a code")
	}
	if !mercadopago.Retryable(err) {
		t.Fatalf("error = %v, want one the queue will retry", err)
	}
	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusPendingPayment {
		t.Fatalf("status = %q, want the order left pending for the retry", stored.Status)
	}
	if stock := h.stock(t); stock.Reserved != 1 {
		t.Fatalf("reserved = %d, want the hold kept while the charge is retried", stock.Reserved)
	}
}

func TestApprovedPaymentSellsTheHeldStock(t *testing.T) {
	h := newHarness(t, 10)
	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)

	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	stored := h.order(t, item.ID)
	if stored.Status != orderdomain.StatusPaid || stored.PaidAt == nil {
		t.Fatalf("order = %q, paidAt = %v", stored.Status, stored.PaidAt)
	}
	stock := h.stock(t)
	if stock.Sold != 2 || stock.Reserved != 0 {
		t.Fatalf("sold = %d, reserved = %d, want 2 and 0", stock.Sold, stock.Reserved)
	}
}

// TestRedeliveredApprovalMovesStockOnce is the idempotency guarantee the whole
// webhook path rests on: Mercado Pago delivers the same event repeatedly, and
// two of those deliveries may be processed at the same time.
func TestRedeliveredApprovalMovesStockOnce(t *testing.T) {
	h := newHarness(t, 10)
	item := h.pendingOrder(t, 3)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)

	var wait sync.WaitGroup
	errs := make([]error, 5)
	wait.Add(len(errs))
	for index := range errs {
		go func(index int) {
			defer wait.Done()
			errs[index] = h.service.SyncPayment(context.Background(), paymentID)
		}(index)
	}
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d error = %v", index, err)
		}
	}
	stock := h.stock(t)
	if stock.Sold != 3 || stock.Reserved != 0 {
		t.Fatalf("sold = %d, reserved = %d after five concurrent deliveries, want 3 and 0", stock.Sold, stock.Reserved)
	}
}

func TestRejectedPaymentReturnsTheStock(t *testing.T) {
	h := newHarness(t, 10)
	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusRejected, "cc_rejected_other_reason")

	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusFailed {
		t.Fatalf("status = %q, want failed", stored.Status)
	}
	stock := h.stock(t)
	if stock.Reserved != 0 || stock.Sold != 0 || stock.Available() != 10 {
		t.Fatalf("reserved=%d sold=%d available=%d, want the tickets back on sale", stock.Reserved, stock.Sold, stock.Available())
	}
}

func TestExpiredPixReturnsTheStock(t *testing.T) {
	h := newHarness(t, 6)
	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	// An unpaid PIX that ran out its clock arrives as cancelled/expired.
	h.provider.move(paymentID, mercadopago.StatusCancelled, mercadopago.DetailExpired)

	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusExpired {
		t.Fatalf("status = %q, want expired", stored.Status)
	}
	if stock := h.stock(t); stock.Available() != 6 {
		t.Fatalf("available = %d, want 6", stock.Available())
	}
}

func TestPendingChargeMovesNothing(t *testing.T) {
	h := newHarness(t, 10)
	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)

	for _, status := range []string{mercadopago.StatusPending, mercadopago.StatusInProcess, mercadopago.StatusInMediation} {
		h.provider.move(paymentID, status, "")
		if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
			t.Fatalf("SyncPayment(%s) error = %v", status, err)
		}
		stored := h.order(t, item.ID)
		if stored.Status != orderdomain.StatusPendingPayment {
			t.Fatalf("status = %q after %s, want the order untouched", stored.Status, status)
		}
		if stock := h.stock(t); stock.Reserved != 2 || stock.Sold != 0 {
			t.Fatalf("reserved=%d sold=%d after %s; money that is not ours must move nothing", stock.Reserved, stock.Sold, status)
		}
	}
}

func TestRefundAfterPaymentReturnsSoldStock(t *testing.T) {
	h := newHarness(t, 5)
	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	if err := h.service.Refund(context.Background(), item.ID); err != nil {
		t.Fatalf("Refund() error = %v", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefunded {
		t.Fatalf("status = %q, want refunded", stored.Status)
	}
	stock := h.stock(t)
	if stock.Sold != 0 || stock.Available() != 5 {
		t.Fatalf("sold = %d, available = %d, want the tickets back", stock.Sold, stock.Available())
	}
}

func TestChargebackReturnsSoldStock(t *testing.T) {
	h := newHarness(t, 5)
	item := h.pendingOrder(t, 1)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	h.provider.move(paymentID, mercadopago.StatusChargedBack, "")
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment(charged_back) error = %v", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefunded {
		t.Fatalf("status = %q, want refunded: a chargeback is money pulled back", stored.Status)
	}
	if stock := h.stock(t); stock.Sold != 0 {
		t.Fatalf("sold = %d, want 0", stock.Sold)
	}
}

// TestLatePaymentTakesStockBackWhenItCan covers the buyer who pays after their
// hold lapsed while tickets are still available.
func TestLatePaymentTakesStockBackWhenItCan(t *testing.T) {
	h := newHarness(t, 10)
	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	h.expire(t, item)
	if stock := h.stock(t); stock.Available() != 10 {
		t.Fatalf("available = %d before the late payment, want 10", stock.Available())
	}

	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusPaid {
		t.Fatalf("status = %q, want paid: the tickets were still there", stored.Status)
	}
	stock := h.stock(t)
	if stock.Sold != 2 || stock.Reserved != 0 {
		t.Fatalf("sold = %d, reserved = %d, want 2 and 0", stock.Sold, stock.Reserved)
	}
}

// TestLatePaymentWithoutStockOwesARefund is the case a system that quietly
// marks the order paid would turn into an oversold event.
func TestLatePaymentWithoutStockOwesARefund(t *testing.T) {
	h := newHarness(t, 2)
	ctx := context.Background()
	item := h.pendingOrder(t, 2)
	paymentID := h.charge(t, item)
	h.expire(t, item)

	// Someone else bought the last two while this buyer was away.
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

	stored := h.order(t, item.ID)
	if stored.Status != orderdomain.StatusRefundRequired {
		t.Fatalf("status = %q, want %q", stored.Status, orderdomain.StatusRefundRequired)
	}
	stock := h.stock(t)
	if stock.Sold != 2 || stock.Reserved != 0 {
		t.Fatalf("sold = %d, reserved = %d: the event was oversold", stock.Sold, stock.Reserved)
	}
}

func TestApprovalAfterRefundIsRecordedButNotApplied(t *testing.T) {
	h := newHarness(t, 5)
	item := h.pendingOrder(t, 1)
	paymentID := h.charge(t, item)
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}
	if err := h.service.Refund(context.Background(), item.ID); err != nil {
		t.Fatalf("Refund() error = %v", err)
	}

	// An out-of-order redelivery of the original approval.
	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v, want the stale event swallowed", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusRefunded {
		t.Fatalf("status = %q, want refunded: a refunded order cannot become paid again", stored.Status)
	}
	if stock := h.stock(t); stock.Sold != 0 {
		t.Fatalf("sold = %d, want 0", stock.Sold)
	}
}

func TestSyncIgnoresAPaymentThatBelongsToNobody(t *testing.T) {
	h := newHarness(t, 5)
	h.provider.mu.Lock()
	h.provider.payments["7777777777"] = map[string]any{
		"id": 7777777777, "status": mercadopago.StatusApproved, "external_reference": "ord_from_another_app",
	}
	h.provider.mu.Unlock()

	// Another application may share the provider account. Acknowledging and
	// ignoring is correct; retrying would never find an order.
	if err := h.service.SyncPayment(context.Background(), "7777777777"); err != nil {
		t.Fatalf("SyncPayment() error = %v, want nil", err)
	}
	if stock := h.stock(t); stock.Sold != 0 {
		t.Fatalf("sold = %d, want 0", stock.Sold)
	}
}

func TestChargeBornApprovedSettlesImmediately(t *testing.T) {
	h := newHarness(t, 4)
	h.provider.bornAs = mercadopago.StatusApproved
	item := h.pendingOrder(t, 1)

	if err := h.service.CreateCharge(context.Background(), item.ID); err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}

	if stored := h.order(t, item.ID); stored.Status != orderdomain.StatusPaid {
		t.Fatalf("status = %q, want paid without waiting for a webhook", stored.Status)
	}
	if stock := h.stock(t); stock.Sold != 1 {
		t.Fatalf("sold = %d, want 1", stock.Sold)
	}
}

func TestWebhookBeforeChargeResponseIsPersistedStillSettles(t *testing.T) {
	// The genuine race: Mercado Pago's notification can arrive before the
	// charge response was written to the order. The order is found by the
	// charge's external reference instead.
	h := newHarness(t, 5)
	item := h.pendingOrder(t, 1)

	// Create the charge at the provider directly, as if the response were lost
	// on the way back.
	h.provider.mu.Lock()
	h.provider.nextID++
	paymentID := strconv.FormatInt(h.provider.nextID, 10)
	h.provider.payments[paymentID] = map[string]any{
		"id": h.provider.nextID, "status": mercadopago.StatusApproved, "status_detail": mercadopago.DetailAccredited,
		"external_reference": item.ID, "payment_method_id": "pix", "transaction_amount": 240.0,
	}
	h.provider.mu.Unlock()

	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	stored := h.order(t, item.ID)
	if stored.Status != orderdomain.StatusPaid || stored.PaymentID != paymentID {
		t.Fatalf("order = %q with payment %q, want paid and tied to %s", stored.Status, stored.PaymentID, paymentID)
	}
}

// TestChargeAnsweredAfterTheHoldExpiredDoesNotResurrectTheOrder is the race
// that concurrent workers made reachable, and it is a money bug.
//
// Creating a charge is a round trip to Mercado Pago, and the order can move
// during it — most often because the hold expired and the tickets went back on
// sale, which is exactly what happens when the provider is slow enough for the
// job to be retried for half an hour. The copy of the order read BEFORE that
// round trip is stale by the time it comes back, and writing it back would put
// the order in pending_payment again while its stock belongs to another buyer:
// an oversell that no later step can detect.
func TestChargeAnsweredAfterTheHoldExpiredDoesNotResurrectTheOrder(t *testing.T) {
	h := newHarness(t, 3)
	item := h.pendingOrder(t, 1)

	var expireErr error
	h.provider.duringCreate = func() { expireErr = h.expireOrder(item.ID) }

	if err := h.service.CreateCharge(context.Background(), item.ID); err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	if expireErr != nil {
		t.Fatalf("the hold could not be expired mid-flight: %v", expireErr)
	}

	stored := h.order(t, item.ID)
	if stored.Status != orderdomain.StatusExpired {
		t.Fatalf("status = %q, want expired: a charge answered after the hold lapsed must not resurrect the order", stored.Status)
	}
	if stored.PaymentID == "" {
		t.Fatalf("payment id was not recorded; a buyer who pays this PIX code late could never be matched to the order")
	}
	stock := h.stock(t)
	if stock.Reserved != 0 || stock.Sold != 0 {
		t.Fatalf("stock = reserved %d, sold %d; want the ticket back on sale and held by nobody", stock.Reserved, stock.Sold)
	}

	// And the recorded payment id is what makes the late payment recoverable:
	// paying it now takes the stock back rather than being lost.
	h.provider.move(stored.PaymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), stored.PaymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}
	if settled := h.order(t, item.ID); settled.Status != orderdomain.StatusPaid {
		t.Fatalf("status = %q, want paid: the tickets were available again", settled.Status)
	}
	if stock := h.stock(t); stock.Sold != 1 || stock.Reserved != 0 {
		t.Fatalf("stock = sold %d, reserved %d; want exactly the late payment's ticket sold", stock.Sold, stock.Reserved)
	}
}

// TestStaleChargeResponseDoesNotOverwriteASettledOrder covers the other order
// of the same race: the webhook wins, and the provider's older answer to
// "create the charge" arrives afterwards. Older news must not be recorded over
// newer.
func TestStaleChargeResponseDoesNotOverwriteASettledOrder(t *testing.T) {
	h := newHarness(t, 3)
	item := h.pendingOrder(t, 1)
	paymentID := h.charge(t, item)

	h.provider.move(paymentID, mercadopago.StatusApproved, mercadopago.DetailAccredited)
	if err := h.service.SyncPayment(context.Background(), paymentID); err != nil {
		t.Fatalf("SyncPayment() error = %v", err)
	}

	// The create call's own response, still pending, finally lands.
	stale := &paymentdomain.Charge{
		ID:       paymentID,
		Provider: paymentdomain.ProviderMercadoPago,
		Status:   paymentdomain.StatusPending,
		Method:   paymentdomain.MethodPix,
	}
	if err := h.service.settle(context.Background(), item.ID, stale); err != nil {
		t.Fatalf("settle() error = %v", err)
	}

	stored := h.order(t, item.ID)
	if stored.Status != orderdomain.StatusPaid {
		t.Fatalf("status = %q, want paid", stored.Status)
	}
	if stored.PaymentStatus != paymentdomain.StatusPaid {
		t.Fatalf("payment status = %q, want paid: a stale pending response must not overwrite the settled one", stored.PaymentStatus)
	}
	if stock := h.stock(t); stock.Sold != 1 {
		t.Fatalf("sold = %d, want 1", stock.Sold)
	}
}

func TestReconcileSchedulesEveryPendingOrder(t *testing.T) {
	h := newHarness(t, 10)
	charged := h.pendingOrder(t, 1)
	h.charge(t, charged)
	uncharged := h.pendingOrder(t, 1)
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM jobs WHERE payload->>'orderId' IN ?", []string{charged.ID, uncharged.ID})
	})

	// Other packages may have pending orders of their own at the same moment,
	// so the count is a floor and the assertion is on this test's two orders.
	scheduled, err := h.service.Reconcile(context.Background(), 500)

	if err != nil || scheduled < 2 {
		t.Fatalf("Reconcile() = (%d, %v), want at least 2", scheduled, err)
	}
	var types []string
	if err := h.db.Raw("SELECT type FROM jobs WHERE payload->>'orderId' IN ? ORDER BY type", []string{charged.ID, uncharged.ID}).Scan(&types).Error; err != nil {
		t.Fatalf("read jobs: %v", err)
	}
	if fmt.Sprint(types) != "[charge.create payment.sync]" {
		t.Fatalf("scheduled jobs = %v, want a charge for the uncharged order and a sync for the charged one", types)
	}
}

func TestReconcileSkipsOpenJobsWithoutStarvingOlderPayments(t *testing.T) {
	h := newHarness(t, 4)
	orders := make([]*orderdomain.Order, 0, 4)
	for index := 0; index < 4; index++ {
		orders = append(orders, h.pendingOrder(t, 1))
	}
	ids := []string{orders[0].ID, orders[1].ID, orders[2].ID, orders[3].ID}
	t.Cleanup(func() { h.db.Exec("DELETE FROM jobs WHERE payload->>'orderId' IN ?", ids) })

	// Put these ahead of any unrelated rows in the shared integration database.
	h.db.Exec("UPDATE orders SET updated_at = ? WHERE id IN ?", time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), ids)
	for _, item := range orders[:2] {
		added, err := h.service.ScheduleReconciliation(context.Background(), item.ID)
		if err != nil || !added {
			t.Fatalf("prime open reconciliation for %s = (%v, %v)", item.ID, added, err)
		}
	}

	// The first page is occupied by open jobs. The sweep must continue to the
	// next page and schedule the other two, not revisit page one forever.
	scheduled, err := h.service.Reconcile(context.Background(), 2)
	if err != nil || scheduled != 2 {
		t.Fatalf("Reconcile() = (%d, %v), want the two unscheduled orders", scheduled, err)
	}
	for _, item := range orders {
		var count int64
		h.db.Raw("SELECT COUNT(*) FROM jobs WHERE dedupe_key = ? AND status IN ('pending','processing')",
			queuedomain.TypeCreateCharge+":"+item.ID).Scan(&count)
		if count != 1 {
			t.Fatalf("open recovery jobs for %s = %d, want 1", item.ID, count)
		}
	}
}
