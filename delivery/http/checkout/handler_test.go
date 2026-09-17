package checkout

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	authdomain "vozkot/domain/auth"
	idempotencydomain "vozkot/domain/idempotency"
	"vozkot/domain/pricing"
	ticketdomain "vozkot/domain/ticket"
	idempotencyRepository "vozkot/infra/repositories/idempotency"
	orderRepository "vozkot/infra/repositories/order"
	queueRepository "vozkot/infra/repositories/queue"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
	"vozkot/infra/uow"
	checkoutUsecase "vozkot/usecases/checkout"
	paymentUsecase "vozkot/usecases/payment"
	queueUsecase "vozkot/usecases/queue"
)

// The idempotency contract is an HTTP contract, so it is tested at HTTP: real
// handler, real PostgreSQL behind the key store and the order table, and a
// session injected the way the auth middleware would.

type harness struct {
	db       *gorm.DB
	mux      *http.ServeMux
	keys     idempotencydomain.Store
	ticketID string
	userID   string
	// priceCents is the seeded tier's face value and fee the commission the
	// handler's box office adds, so a test can state the expected split
	// without repeating either number.
	priceCents int64
	fee        pricing.Fee
}

func newHarness(t *testing.T, capacity int) *harness {
	t.Helper()
	return newHarnessWithFee(t, capacity, pricing.Fee{})
}

// newHarnessWithFee is the same API, served by a box office that charges a
// commission. The fee is a parameter rather than a second harness so the
// seeding and cleanup below have exactly one definition.
func newHarnessWithFee(t *testing.T, capacity int, fee pricing.Fee) *harness {
	t.Helper()
	db := testsupport.Database(t)
	ctx := context.Background()

	tickets := ticketRepository.NewTicketRepository(db)
	orders := orderRepository.NewOrderRepository(db)
	keys := idempotencyRepository.NewIdempotencyRepository(db)
	dispatcher := queueUsecase.NewDispatcher(nil)

	userID := testsupport.Unique("usr")
	if err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Checkout HTTP', ?, 'x', 'user', 0, NOW(), NOW())`, userID, userID+"@vozkot.test").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	eventID := testsupport.SeedEvent(t, db, userID)
	ticket, err := ticketdomain.New(testsupport.Unique("tkt"), userID, ticketdomain.Draft{
		EventID: eventID, Title: "Pista", PriceCents: 24000, Quantity: capacity,
		Status: ticketdomain.StatusOnSale,
	}, time.Now())
	if err != nil {
		t.Fatalf("build ticket: %v", err)
	}
	if err := tickets.Create(ctx, ticket); err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM jobs WHERE payload->>'orderId' IN (SELECT order_id FROM order_items WHERE ticket_id = ?)", ticket.ID)
		db.Exec("DELETE FROM orders WHERE id IN (SELECT order_id FROM order_items WHERE ticket_id = ?)", ticket.ID)
		db.Exec("DELETE FROM tickets WHERE id = ?", ticket.ID)
		db.Exec("DELETE FROM users WHERE id = ?", userID)
	})

	checkout := checkoutUsecase.NewService(uow.NewRunner(db), orders, tickets, nil, dispatcher, checkoutUsecase.Settings{
		HoldFor:     30 * time.Minute,
		CartHoldFor: 10 * time.Minute,
		Fee:         fee,
	})
	payments := paymentUsecase.NewService(uow.NewRunner(db), orders, nil, queueRepository.NewJobRepository(db), dispatcher, nil)

	mux := http.NewServeMux()
	NewHandler(checkout, payments, nil, nil, keys, idempotencydomain.DefaultLease).Register(mux)
	return &harness{
		db: db, mux: mux, keys: keys,
		ticketID: ticket.ID, userID: userID, priceCents: ticket.PriceCents, fee: fee,
	}
}

func (h *harness) post(t *testing.T, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/checkout", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	// What the auth middleware would have put on the context.
	request = request.WithContext(authdomain.WithClaims(request.Context(), &authdomain.Claims{
		UserID: h.userID, Role: "user",
	}))
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

func (h *harness) body(quantity int) string {
	return `{"ticketId":"` + h.ticketID + `","quantity":` + strconv.Itoa(quantity) +
		`,"buyer":{"name":"Maria Souza","email":"maria@exemplo.com.br","document":"12345678909"}}`
}

func (h *harness) reserved(t *testing.T) int {
	t.Helper()
	var reserved int
	if err := h.db.Raw("SELECT reserved FROM tickets WHERE id = ?", h.ticketID).Scan(&reserved).Error; err != nil {
		t.Fatalf("read reserved: %v", err)
	}
	return reserved
}

func TestCheckoutRequiresAnIdempotencyKey(t *testing.T) {
	h := newHarness(t, 10)

	response := h.post(t, "", h.body(1))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a payment endpoint must not accept unkeyed requests", response.Code)
	}
	if h.reserved(t) != 0 {
		t.Fatal("a refused request reserved stock")
	}
}

func TestCheckoutReplaysTheSameResponseForTheSameKey(t *testing.T) {
	h := newHarness(t, 10)
	key := testsupport.Unique("key")

	first := h.post(t, key, h.body(2))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}

	// The phone lost signal and the client retried. Same key, same body.
	second := h.post(t, key, h.body(2))

	if second.Code != http.StatusCreated {
		t.Fatalf("replay status = %d, want the original 201", second.Code)
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("replay was not marked as one")
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay body differs:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	if got := h.reserved(t); got != 2 {
		t.Fatalf("reserved = %d, want 2: the retry must not hold a second batch", got)
	}
}

func TestCheckoutRefusesAKeyReusedWithADifferentBody(t *testing.T) {
	h := newHarness(t, 10)
	key := testsupport.Unique("key")
	if response := h.post(t, key, h.body(1)); response.Code != http.StatusCreated {
		t.Fatalf("first status = %d", response.Code)
	}

	response := h.post(t, key, h.body(3))

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: replaying the first response would hide a client bug", response.Code)
	}
	if got := h.reserved(t); got != 1 {
		t.Fatalf("reserved = %d, want 1", got)
	}
}

// TestConcurrentRetriesWithOneKeyReserveOnce is the race the key exists for:
// the same request in flight several times at once.
func TestConcurrentRetriesWithOneKeyReserveOnce(t *testing.T) {
	h := newHarness(t, 10)
	key := testsupport.Unique("key")

	var wait sync.WaitGroup
	codes := make([]int, 8)
	for index := range codes {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			codes[index] = h.post(t, key, h.body(2)).Code
		}(index)
	}
	wait.Wait()

	created := 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			// "Still in progress"; the honest answer for a loser of the race.
		default:
			t.Fatalf("unexpected status %d among %v", code, codes)
		}
	}
	if created < 1 {
		t.Fatalf("no request succeeded: %v", codes)
	}
	if got := h.reserved(t); got != 2 {
		t.Fatalf("reserved = %d, want exactly one batch of 2 across %d concurrent retries", got, len(codes))
	}
}

func TestCheckoutAnswersSoldOutWithConflict(t *testing.T) {
	h := newHarness(t, 1)
	if response := h.post(t, testsupport.Unique("key"), h.body(1)); response.Code != http.StatusCreated {
		t.Fatalf("first status = %d", response.Code)
	}

	response := h.post(t, testsupport.Unique("key"), h.body(1))

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a sold-out tier", response.Code)
	}
}

func TestCheckoutValidatesTheBuyer(t *testing.T) {
	h := newHarness(t, 10)
	key := testsupport.Unique("key")

	response := h.post(t, key,
		`{"ticketId":"`+h.ticketID+`","quantity":1,"buyer":{"name":"","email":"nope","document":""}}`)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", response.Code)
	}
	if h.reserved(t) != 0 {
		t.Fatal("an invalid request reserved stock")
	}
	// A failed attempt releases its key, so the corrected retry is not told for
	// a day that the request is still in progress.
	//
	// Counted for THIS key only. The table is shared with every other test in
	// the package, and the recovery tests leave orphaned claims in `processing`
	// on purpose: a count across the whole table would be measuring them.
	var count int64
	h.db.Raw("SELECT COUNT(*) FROM idempotency_keys WHERE key = ? AND scope = 'checkout' AND state = 'processing'", key).Scan(&count)
	if count != 0 {
		t.Fatalf("the key was left in processing after a failure; the corrected retry would be refused for a day")
	}
}

func TestOrdersAreScopedToTheirBuyer(t *testing.T) {
	h := newHarness(t, 10)
	created := h.post(t, testsupport.Unique("key"), h.body(1))
	var envelope OrderEnvelope
	if err := json.Unmarshal(created.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/orders/"+envelope.Data.ID, nil)
	request = request.WithContext(authdomain.WithClaims(request.Context(), &authdomain.Claims{
		UserID: "usr_someone_else", Role: "user",
	}))
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a buyer must not read another buyer's order", recorder.Code)
	}
}

// The split has to survive the last hop as well.
//
// Everything below the handler is covered by usecases/checkout's money tests;
// what those cannot see is the DTO. A checkout screen builds its price
// breakdown from these three fields, so a mapper that sent the total twice, or
// sent the face value as the total, would show the buyer a number they did not
// agree to pay while every test underneath still passed.
func TestCheckoutResponseCarriesTheFeeSplit(t *testing.T) {
	fee, err := pricing.PlatformFee()
	if err != nil {
		t.Fatalf("PlatformFee(): %v", err)
	}
	h := newHarnessWithFee(t, 10, fee)

	const quantity = 2
	wantFace := h.priceCents * quantity
	wantFee := fee.On(h.priceCents) * quantity

	recorder := h.post(t, testsupport.Unique("key"), h.body(quantity))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /checkout = %d, want 201: %s", recorder.Code, recorder.Body.String())
	}

	var envelope struct {
		Data OrderResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	order := envelope.Data

	if order.SubtotalCents != wantFace {
		t.Errorf("subtotalCents = %d, want %d", order.SubtotalCents, wantFace)
	}
	if order.ServiceFeeCents != wantFee {
		t.Errorf("serviceFeeCents = %d, want %d", order.ServiceFeeCents, wantFee)
	}
	if order.TotalCents != wantFace+wantFee {
		t.Errorf("totalCents = %d, want %d", order.TotalCents, wantFace+wantFee)
	}
	// The three have to be mutually consistent in the payload itself, because
	// that is the arithmetic the screen will repeat to the buyer.
	if order.TotalCents != order.SubtotalCents+order.ServiceFeeCents {
		t.Errorf("the response does not add up: %d != %d + %d",
			order.TotalCents, order.SubtotalCents, order.ServiceFeeCents)
	}
	if order.ServiceFeeCents == 0 {
		t.Error("serviceFeeCents is zero on a feed box office; the rate did not reach the API")
	}

	// And the per-ticket fee on the line, which is what the event page quoted.
	if len(order.Items) != 1 {
		t.Fatalf("response has %d lines, want 1", len(order.Items))
	}
	if got := order.Items[0].UnitFeeCents; got != fee.On(h.priceCents) {
		t.Errorf("line unitFeeCents = %d, want %d", got, fee.On(h.priceCents))
	}
}

// Without a fee the response must show no fee line at all, rather than a zero
// that a screen would still render as "service fee R$ 0,00".
func TestCheckoutResponseShowsNoFeeWhenThereIsNone(t *testing.T) {
	h := newHarnessWithFee(t, 10, pricing.Fee{})

	recorder := h.post(t, testsupport.Unique("key"), h.body(2))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /checkout = %d, want 201: %s", recorder.Code, recorder.Body.String())
	}

	var envelope struct {
		Data OrderResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	order := envelope.Data

	if order.ServiceFeeCents != 0 {
		t.Errorf("serviceFeeCents = %d with no fee configured, want 0", order.ServiceFeeCents)
	}
	if order.TotalCents != order.SubtotalCents {
		t.Errorf("total %d is not the subtotal %d with no fee", order.TotalCents, order.SubtotalCents)
	}
	if order.TotalCents != h.priceCents*2 {
		t.Errorf("total = %d, want the tier price %d", order.TotalCents, h.priceCents*2)
	}
}
