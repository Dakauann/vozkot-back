package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	payoutHTTP "vozkot/delivery/http/payout"
	authdomain "vozkot/domain/auth"
	ledgerdomain "vozkot/domain/ledger"
	"vozkot/infra/mercadopago"
	"vozkot/infra/testsupport"
	"vozkot/infra/uow"
	payoutUsecase "vozkot/usecases/payout"
)

// The whole path, once: a buyer pays, and the organiser reads their balance
// over HTTP.
//
// Every other test in this feature stops at a seam: the settle tests read the
// repository directly, the handler tests seed the ledger directly, and a seam
// is exactly where two correct halves disagree. This is the only test that
// would catch the settle path writing entries the balance endpoint does not
// count, or the endpoint reading a column the accrual does not fill.
func TestABuyerPaysAndTheOrganiserSeesItOverHTTP(t *testing.T) {
	h := newHarness(t, 8)
	ctx := context.Background()

	payouts := payoutUsecase.NewService(
		uow.NewRunner(h.db), payoutUsecase.AlwaysStandard,
		ledgerdomain.BankingCalendar, time.Now, testsupport.Unique,
	)
	h.service.WithLedger(payouts)
	t.Cleanup(func() { h.db.Exec("DELETE FROM ledger_entries WHERE organiser_id = ?", h.ownerID) })

	router := http.NewServeMux()
	payoutHTTP.NewHandler(payouts).Register(router)

	read := func(target, who string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if who != "" {
			request = request.WithContext(authdomain.WithClaims(
				request.Context(), &authdomain.Claims{UserID: who, Role: "user"}))
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	// Nothing sold yet: a real zero, not an error.
	var before payoutHTTP.BalanceResponse
	if err := json.Unmarshal(read("/api/v1/organiser/balance", h.ownerID).Body.Bytes(), &before); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if before.TotalCents != 0 {
		t.Fatalf("balance before any sale = %d", before.TotalCents)
	}

	// Two buyers pay.
	first := h.pendingOrder(t, 2)
	h.pay(t, first)
	second := h.pendingOrder(t, 1)
	secondPayment := h.pay(t, second)
	sold := h.order(t, first.ID).SubtotalCents + h.order(t, second.ID).SubtotalCents

	var after payoutHTTP.BalanceResponse
	if err := json.Unmarshal(read("/api/v1/organiser/balance", h.ownerID).Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.TotalCents != sold {
		t.Fatalf("balance = %d over HTTP, want the %d that was sold", after.TotalCents, sold)
	}
	// Owed, but not yet payable: the show has not happened.
	if after.AvailableCents != 0 {
		t.Errorf("available = %d before the event", after.AvailableCents)
	}
	if after.PendingCents+after.ReservedCents != sold {
		t.Errorf("pending %d + reserved %d does not account for %d",
			after.PendingCents, after.ReservedCents, sold)
	}
	if after.Currency != "BRL" {
		t.Errorf("currency = %q", after.Currency)
	}

	// The statement shows the movements behind that number.
	var statement payoutHTTP.LedgerResponse
	if err := json.Unmarshal(read("/api/v1/organiser/ledger", h.ownerID).Body.Bytes(), &statement); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if statement.Total != 4 {
		t.Fatalf("statement has %d entries, want a sale and a reserve for each of two orders", statement.Total)
	}

	// One buyer is refunded, and the organiser's balance follows immediately.
	h.provider.move(secondPayment, mercadopago.StatusRefunded, "")
	if err := h.service.SyncPayment(ctx, secondPayment); err != nil {
		t.Fatalf("SyncPayment(refunded): %v", err)
	}
	var settled payoutHTTP.BalanceResponse
	if err := json.Unmarshal(read("/api/v1/organiser/balance", h.ownerID).Body.Bytes(), &settled); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := h.order(t, first.ID).SubtotalCents; settled.TotalCents != want {
		t.Fatalf("balance after the refund = %d, want only the order that stands: %d",
			settled.TotalCents, want)
	}
	// The refund is payable NOW while the sale it reverses is not, so the
	// payable figure has gone negative. That is the property the whole ledger
	// exists for, and this is the only place it is seen end to end.
	if settled.AvailableCents >= 0 {
		t.Fatalf("available = %d after a refund; the reversal did not outrun its sale",
			settled.AvailableCents)
	}

	// And none of it is readable without being the organiser.
	if code := read("/api/v1/organiser/balance", "").Code; code != http.StatusUnauthorized {
		t.Errorf("balance without a session = %d, want 401", code)
	}
	stranger := testsupport.Unique("usr")
	var theirs payoutHTTP.BalanceResponse
	if err := json.Unmarshal(read("/api/v1/organiser/balance", stranger).Body.Bytes(), &theirs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if theirs.TotalCents != 0 {
		t.Fatalf("another account read %d of this organiser's money", theirs.TotalCents)
	}
}
