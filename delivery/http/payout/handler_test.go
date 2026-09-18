package payout

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authdomain "vozkot/domain/auth"
	ledgerdomain "vozkot/domain/ledger"
	ledgerRepository "vozkot/infra/repositories/ledger"
	"vozkot/infra/testsupport"
	"vozkot/infra/uow"
	payoutUsecase "vozkot/usecases/payout"
)

// A balance is money, so the two things worth proving at this layer are that a
// stranger cannot read one and that an organiser reads their OWN, never the
// one named by whatever they put in the query string.
func serve(t *testing.T) (*Handler, string, string) {
	t.Helper()
	db := testsupport.Database(t)
	mine := testsupport.Unique("org")
	theirs := testsupport.Unique("org")
	entries := ledgerRepository.NewLedgerRepository(db)
	now := time.Now()

	err := entries.Append(context.Background(), []ledgerdomain.Entry{
		{ID: testsupport.Unique("led"), OrganiserID: mine, EventID: "evt_mine", OrderID: "ord_1",
			Kind: ledgerdomain.KindSale, AmountCents: 4_806_00,
			AvailableAt: now.Add(-time.Hour), CreatedAt: now},
		{ID: testsupport.Unique("led"), OrganiserID: mine, EventID: "evt_mine", OrderID: "ord_1",
			Kind: ledgerdomain.KindReserve, AmountCents: 534_00,
			AvailableAt: now.Add(720 * time.Hour), CreatedAt: now},
		{ID: testsupport.Unique("led"), OrganiserID: theirs, EventID: "evt_theirs", OrderID: "ord_2",
			Kind: ledgerdomain.KindSale, AmountCents: 999_00,
			AvailableAt: now.Add(-time.Hour), CreatedAt: now},
	})
	if err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM ledger_entries WHERE organiser_id IN (?, ?)", mine, theirs)
	})

	service := payoutUsecase.NewService(
		uow.NewRunner(db), payoutUsecase.AlwaysStandard,
		ledgerdomain.BankingCalendar, time.Now, testsupport.Unique,
	)
	return NewHandler(service), mine, theirs
}

func call(t *testing.T, handler *Handler, target, organiserID string) *httptest.ResponseRecorder {
	t.Helper()
	router := http.NewServeMux()
	handler.Register(router)
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if organiserID != "" {
		// Through the claims, the way the session middleware does it, so the
		// test exercises the same path a real request takes rather than a
		// shortcut that could keep passing after the middleware changed.
		request = request.WithContext(authdomain.WithClaims(
			request.Context(), &authdomain.Claims{UserID: organiserID, Role: "user"}))
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestABalanceIsRefusedWithoutASession(t *testing.T) {
	handler, _, _ := serve(t)
	for _, target := range []string{"/api/v1/organiser/balance", "/api/v1/organiser/ledger"} {
		if got := call(t, handler, target, "").Code; got != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", target, got)
		}
	}
}

func TestAnOrganiserSeesTheirOwnBalanceAndOnlyTheirs(t *testing.T) {
	handler, mine, theirs := serve(t)

	recorder := call(t, handler, "/api/v1/organiser/balance", mine)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var balance BalanceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &balance); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if balance.AvailableCents != 4_806_00 {
		t.Errorf("available = %d", balance.AvailableCents)
	}
	if balance.ReservedCents != 534_00 {
		t.Errorf("reserved = %d", balance.ReservedCents)
	}
	if balance.TotalCents != 5_340_00 {
		t.Errorf("total = %d, want the whole claim", balance.TotalCents)
	}
	// The other organiser's sale is nowhere in it.
	if balance.TotalCents == 5_340_00+999_00 {
		t.Fatal("the balance included another organiser's sale")
	}

	// And the query string cannot be used to ask for somebody else's: the id
	// comes from the session, so a tampered parameter changes nothing.
	tampered := call(t, handler, "/api/v1/organiser/balance?organiserId="+theirs, mine)
	if tampered.Body.String() != recorder.Body.String() {
		t.Fatal("a query parameter changed whose balance was returned")
	}
}

func TestTheExtractIsScopedAndFilterable(t *testing.T) {
	handler, mine, _ := serve(t)

	recorder := call(t, handler, "/api/v1/organiser/ledger", mine)
	var page LedgerResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 2 || len(page.Data) != 2 {
		t.Fatalf("entries = %d (total %d), want this organiser's two", len(page.Data), page.Total)
	}
	for _, entry := range page.Data {
		if entry.EventID != "evt_mine" {
			t.Errorf("entry from %q leaked into the extract", entry.EventID)
		}
	}
	// Filtering by an event this organiser does not own returns nothing rather
	// than that event's rows.
	filtered := call(t, handler, "/api/v1/organiser/ledger?eventId=evt_theirs", mine)
	if err := json.Unmarshal(filtered.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("filtering by another organiser's event returned %d rows", page.Total)
	}
}
