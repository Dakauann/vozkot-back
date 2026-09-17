package ticket_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ticketHTTP "vozkot/delivery/http/ticket"
	authdomain "vozkot/domain/auth"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
	ticketUsecase "vozkot/usecases/ticket"

	"gorm.io/gorm"
)

// The HTTP surface of a ticket tier, against a real PostgreSQL.
//
// This file exists because of a bug nothing else in the suite could see. The
// create handler read `eventId` off the request and then did not pass it on, so
// every tier creation in the product answered 422 "a ticket tier must belong to
// an event" while the client was sending a perfectly good one. Every usecase
// test constructs CreateInput directly, which is exactly the line that was
// broken — so 517 passing tests said nothing about it.
//
// The lesson generalises: a field that is read at one layer and used at another
// needs a test that crosses the boundary between them.

type harness struct {
	db      *gorm.DB
	mux     *http.ServeMux
	userID  string
	eventID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testsupport.Database(t)

	userID := seedUser(t, db)
	eventID := testsupport.SeedEvent(t, db, userID)

	mux := http.NewServeMux()
	ticketHTTP.NewHandler(ticketUsecase.NewService(ticketRepository.NewTicketRepository(db))).
		Register(mux)

	return &harness{db: db, mux: mux, userID: userID, eventID: eventID}
}

func seedUser(t *testing.T, db *gorm.DB) string {
	t.Helper()
	id := testsupport.Unique("usr")
	err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Ticket HTTP Test', ?, 'x', 'user', 0, NOW(), NOW())`,
		id, id+"@vozkot.test").Error
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM users WHERE id = ?", id) })
	return id
}

func (h *harness) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tickets", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	// What the auth middleware would have put on the context.
	request = request.WithContext(authdomain.WithClaims(request.Context(), &authdomain.Claims{
		UserID: h.userID, Role: "user",
	}))
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

// A tier names the event it sells admission to, and the handler has to carry it
// from the body to the use case.
func TestCreateCarriesTheEventFromTheBody(t *testing.T) {
	h := newHarness(t)

	recorder := h.post(t, `{
		"eventId": "`+h.eventID+`",
		"title": "Plateia VIP",
		"description": "Fileiras da frente.",
		"priceCents": 24000,
		"quantity": 104,
		"status": "on_sale"
	}`)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/tickets = %d, want 201. body: %s",
			recorder.Code, recorder.Body.String())
	}

	var envelope struct {
		Data struct {
			ID       string `json:"id"`
			EventID  string `json:"eventId"`
			Title    string `json:"title"`
			Quantity int    `json:"quantity"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Data.EventID != h.eventID {
		t.Errorf("eventId = %q, want %q", envelope.Data.EventID, h.eventID)
	}
	if envelope.Data.Title != "Plateia VIP" || envelope.Data.Quantity != 104 {
		t.Errorf("tier came back as %+v, want the one that was posted", envelope.Data)
	}
	t.Cleanup(func() { h.db.Exec("DELETE FROM tickets WHERE id = ?", envelope.Data.ID) })
}

// And a tier with no event is still refused, which is the rule the missing
// field was accidentally proving.
func TestCreateRefusesATierWithNoEvent(t *testing.T) {
	h := newHarness(t)

	recorder := h.post(t, `{
		"title": "Plateia",
		"priceCents": 12000,
		"quantity": 10
	}`)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST with no eventId = %d, want 422. body: %s",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "belong to an event") {
		t.Errorf("the error does not name the missing event: %s", recorder.Body.String())
	}
}
