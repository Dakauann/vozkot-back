package asaas

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vozkot/domain/payment"
)

// Asaas is stubbed at the HTTP boundary and nowhere else, so what is under test
// is this adapter AND the wire format together: the header it authenticates
// with, the shape it sends, and the units it speaks.
//
// There is no fake Client. A double would only prove the double agrees with
// itself, and the two things most likely to be wrong here, the reais/centavos
// conversion and the idempotency lookup; are both visible only in the requests
// that actually go out.

type stub struct {
	mu sync.Mutex
	// payments is the provider's state, keyed by id.
	payments map[string]*Payment
	// requests records every path hit, so a test can assert what was NOT called.
	requests []string
	// bodies records the decoded create-payment bodies.
	created []Payment

	customerID  string
	nextID      int
	failCreate  int
	failQRCode  bool
	qrPayload   string
	tokenSeen   string
	searchEmpty bool
}

func newStub() *stub {
	return &stub{
		payments:   map[string]*Payment{},
		customerID: "cus_000001",
		qrPayload:  "00020126-ASAAS-PIX-PAYLOAD",
	}
}

func (s *stub) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, request.Method+" "+request.URL.Path+"?"+request.URL.RawQuery)
		s.tokenSeen = request.Header.Get("access_token")
		response.Header().Set("Content-Type", "application/json")

		path := request.URL.Path
		switch {
		case request.Method == http.MethodGet && path == "/customers":
			if s.searchEmpty {
				_ = json.NewEncoder(response).Encode(list[Customer]{})
				return
			}
			_ = json.NewEncoder(response).Encode(list[Customer]{
				Data: []Customer{{ID: s.customerID, Document: request.URL.Query().Get("cpfCnpj")}},
			})

		case request.Method == http.MethodPost && path == "/customers":
			_ = json.NewEncoder(response).Encode(Customer{ID: s.customerID})

		case request.Method == http.MethodGet && path == "/payments":
			reference := request.URL.Query().Get("externalReference")
			out := list[Payment]{}
			for _, item := range s.payments {
				if item.ExternalReference == reference {
					out.Data = append(out.Data, *item)
				}
			}
			_ = json.NewEncoder(response).Encode(out)

		case request.Method == http.MethodPost && path == "/payments":
			if s.failCreate != 0 {
				response.WriteHeader(s.failCreate)
				_, _ = response.Write([]byte(`{"errors":[{"code":"invalid_value","description":"nope"}]}`))
				return
			}
			var draft Payment
			_ = json.NewDecoder(request.Body).Decode(&draft)
			s.created = append(s.created, draft)
			s.nextID++
			draft.ID = "pay_" + string(rune('a'+s.nextID-1))
			draft.Status = StatusPending
			s.payments[draft.ID] = &draft
			response.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(response).Encode(draft)

		case request.Method == http.MethodGet && strings.HasSuffix(path, "/pixQrCode"):
			if s.failQRCode {
				response.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(response).Encode(PixQRCode{
				EncodedImage: "aGVsbG8=",
				Payload:      s.qrPayload,
			})

		case request.Method == http.MethodGet && strings.HasPrefix(path, "/payments/"):
			id := strings.TrimPrefix(path, "/payments/")
			item, found := s.payments[id]
			if !found {
				response.WriteHeader(http.StatusNotFound)
				_, _ = response.Write([]byte(`{"errors":[{"description":"not found"}]}`))
				return
			}
			_ = json.NewEncoder(response).Encode(item)

		case request.Method == http.MethodPost && strings.HasSuffix(path, "/refund"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/payments/"), "/refund")
			if item, found := s.payments[id]; found {
				item.Status = StatusRefunded
			}
			_, _ = response.Write([]byte(`{}`))

		case request.Method == http.MethodDelete && strings.HasPrefix(path, "/payments/"):
			id := strings.TrimPrefix(path, "/payments/")
			if item, found := s.payments[id]; found {
				item.Deleted = true
			}
			_, _ = response.Write([]byte(`{"deleted":true}`))

		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *stub) calls(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, call := range s.requests {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func gatewayFor(t *testing.T, s *stub) *Gateway {
	t.Helper()
	server := s.server(t)
	return NewGateway(NewClient("test-key", server.URL))
}

func chargeRequest() payment.ChargeRequest {
	return payment.ChargeRequest{
		Method:            payment.MethodPix,
		AmountCents:       10890,
		Currency:          "BRL",
		Description:       "Ingressos",
		ExternalReference: "ord_9f2c1d8a",
		Customer: payment.Customer{
			Name:     "Maria Souza",
			Email:    "maria@exemplo.com.br",
			Document: "529.982.247-25",
		},
	}
}

func TestCreateChargeSendsReaisAndReadsBackCentavos(t *testing.T) {
	s := newStub()
	gateway := gatewayFor(t, s)

	charge, err := gateway.CreateCharge(context.Background(), chargeRequest())

	if err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	// R$ 108,90 on the wire.
	if got := s.created[0].Value; got != 108.90 {
		t.Fatalf("sent value = %v, want 108.90 reais", got)
	}
	// …and integer centavos back, with no drift.
	if charge.AmountCents != 10890 {
		t.Fatalf("amount = %d, want 10890 centavos", charge.AmountCents)
	}
	if charge.Provider != payment.ProviderAsaas {
		t.Fatalf("provider = %q", charge.Provider)
	}
}

// TestAmountsSurviveTheFloatRoundTrip is the money test.
//
// Asaas speaks reais as JSON numbers, which decode into float64. 10.99 decodes
// as 10.989999999999999787, and int64(x*100) on that is 1098, one centavo
// lost per charge, silently. Every value here is one that truncation gets wrong.
func TestAmountsSurviveTheFloatRoundTrip(t *testing.T) {
	for _, cents := range []int64{1, 7, 10, 99, 1099, 1098, 2470, 10890, 99999, 123456789} {
		if got := toCents(toReais(cents)); got != cents {
			t.Fatalf("%d centavos round-tripped to %d", cents, got)
		}
	}
}

// TestCreateChargeIsIdempotentOnTheOrderID is the one that stops a retried job
// charging a buyer twice.
//
// Asaas has no idempotency header, so the guard is a lookup on our own order id
// before anything is created. Without it, a charge job that timed out AFTER the
// charge was created opens a second one on the next attempt: two PIX codes, two
// payable amounts, one set of tickets.
func TestCreateChargeIsIdempotentOnTheOrderID(t *testing.T) {
	s := newStub()
	gateway := gatewayFor(t, s)

	first, err := gateway.CreateCharge(context.Background(), chargeRequest())
	if err != nil {
		t.Fatalf("first CreateCharge() error = %v", err)
	}
	second, err := gateway.CreateCharge(context.Background(), chargeRequest())
	if err != nil {
		t.Fatalf("second CreateCharge() error = %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("retry produced a second charge: %s then %s", first.ID, second.ID)
	}
	if created := s.calls("POST /payments"); created != 1 {
		t.Fatalf("POST /payments called %d times, want 1", created)
	}
}

// TestADeletedChargeIsNotReusedByTheIdempotencyLookup: a voided charge is not
// one the buyer can pay, so finding it must not stop a new one being made.
func TestADeletedChargeIsNotReusedByTheIdempotencyLookup(t *testing.T) {
	s := newStub()
	gateway := gatewayFor(t, s)

	first, err := gateway.CreateCharge(context.Background(), chargeRequest())
	if err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	if err := gateway.CancelCharge(context.Background(), first.ID); err != nil {
		t.Fatalf("CancelCharge() error = %v", err)
	}

	second, err := gateway.CreateCharge(context.Background(), chargeRequest())
	if err != nil {
		t.Fatalf("second CreateCharge() error = %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("a cancelled charge was handed back as if it were payable")
	}
}

func TestPixPayloadIsFetchedAndAttached(t *testing.T) {
	s := newStub()
	gateway := gatewayFor(t, s)

	charge, err := gateway.CreateCharge(context.Background(), chargeRequest())

	if err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	if charge.PixCopyPaste != s.qrPayload {
		t.Fatalf("copy-paste = %q, want the payload from the QR endpoint", charge.PixCopyPaste)
	}
	if charge.PixQRCodeBase64 == "" {
		t.Fatal("no QR image attached")
	}
}

// TestAFailedQRFetchStillReturnsThePayableCharge.
//
// The charge exists and is payable through its invoice URL. Failing the whole
// call over a second HTTP request would cost the buyer a reservation for a
// charge that was successfully created.
func TestAFailedQRFetchStillReturnsThePayableCharge(t *testing.T) {
	s := newStub()
	s.failQRCode = true
	gateway := gatewayFor(t, s)

	charge, err := gateway.CreateCharge(context.Background(), chargeRequest())

	if err != nil {
		t.Fatalf("CreateCharge() error = %v, want the charge despite the QR failure", err)
	}
	if charge.ID == "" {
		t.Fatal("no charge returned")
	}
	if charge.PixCopyPaste != "" {
		t.Fatalf("copy-paste = %q, want it empty", charge.PixCopyPaste)
	}
}

func TestCreateChargeRefusesWhatCannotBeBilled(t *testing.T) {
	s := newStub()
	gateway := gatewayFor(t, s)

	cases := map[string]struct {
		mutate func(*payment.ChargeRequest)
		want   error
	}{
		"no amount":   {func(r *payment.ChargeRequest) { r.AmountCents = 0 }, payment.ErrInvalidAmount},
		"negative":    {func(r *payment.ChargeRequest) { r.AmountCents = -1 }, payment.ErrInvalidAmount},
		"no document": {func(r *payment.ChargeRequest) { r.Customer.Document = " " }, payment.ErrDocumentRequired},
	}
	for name, testCase := range cases {
		request := chargeRequest()
		testCase.mutate(&request)
		if _, err := gateway.CreateCharge(context.Background(), request); !errors.Is(err, testCase.want) {
			t.Fatalf("%s: error = %v, want %v", name, err, testCase.want)
		}
	}
}

func TestGetChargeTranslatesAMissingOne(t *testing.T) {
	s := newStub()
	gateway := gatewayFor(t, s)

	_, err := gateway.GetCharge(context.Background(), "pay_missing")

	if !errors.Is(err, payment.ErrChargeNotFound) {
		t.Fatalf("GetCharge() error = %v, want %v", err, payment.ErrChargeNotFound)
	}
}

// TestGetChargeDoesNotRefetchTheQRCode: the payload never changes and is already
// on the order. Re-fetching it on every reconciliation sweep would double the
// calls this system makes for nothing.
func TestGetChargeDoesNotRefetchTheQRCode(t *testing.T) {
	s := newStub()
	gateway := gatewayFor(t, s)
	created, err := gateway.CreateCharge(context.Background(), chargeRequest())
	if err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	before := s.calls("GET /payments/" + created.ID + "/pixQrCode")

	if _, err := gateway.GetCharge(context.Background(), created.ID); err != nil {
		t.Fatalf("GetCharge() error = %v", err)
	}

	if after := s.calls("GET /payments/" + created.ID + "/pixQrCode"); after != before {
		t.Fatalf("QR endpoint called again on a read: %d then %d", before, after)
	}
}

func TestTheAPIKeyTravelsInAsaasOwnHeader(t *testing.T) {
	s := newStub()
	gateway := gatewayFor(t, s)

	if _, err := gateway.CreateCharge(context.Background(), chargeRequest()); err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	if s.tokenSeen != "test-key" {
		t.Fatalf("access_token header = %q, want the api key", s.tokenSeen)
	}
}

func TestAnUnconfiguredGatewayRefusesRatherThanCalling(t *testing.T) {
	gateway := NewGateway(NewClient("", ""))

	_, err := gateway.CreateCharge(context.Background(), chargeRequest())

	if !errors.Is(err, payment.ErrNotConfigured) {
		t.Fatalf("CreateCharge() error = %v, want %v", err, payment.ErrNotConfigured)
	}
}

// TestProviderFailuresAreClassifiedForTheQueue.
//
// The queue reads payment.Retryable: a refused charge must not be retried eight
// times, and a 502 must be.
func TestProviderFailuresAreClassifiedForTheQueue(t *testing.T) {
	for status, wantRetry := range map[int]bool{
		http.StatusBadRequest:          false,
		http.StatusUnauthorized:        false,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
	} {
		s := newStub()
		s.failCreate = status
		gateway := gatewayFor(t, s)

		_, err := gateway.CreateCharge(context.Background(), chargeRequest())
		if err == nil {
			t.Fatalf("status %d: CreateCharge() succeeded, want a failure", status)
		}
		if got := payment.Retryable(err); got != wantRetry {
			t.Fatalf("status %d: Retryable = %t, want %t", status, got, wantRetry)
		}
	}
}

func TestCustomerIsCreatedWhenTheDocumentIsUnknown(t *testing.T) {
	s := newStub()
	s.searchEmpty = true
	gateway := gatewayFor(t, s)

	if _, err := gateway.CreateCharge(context.Background(), chargeRequest()); err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	if created := s.calls("POST /customers"); created != 1 {
		t.Fatalf("POST /customers called %d times, want 1", created)
	}
}

func TestDueDateNeverPrecedesTheHold(t *testing.T) {
	s := newStub()
	gateway := gatewayFor(t, s)

	request := chargeRequest()
	// A hold well past the default one-day window.
	request.ExpiresAt = time.Now().UTC().AddDate(0, 0, 5)

	if _, err := gateway.CreateCharge(context.Background(), request); err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}
	due, err := time.Parse("2006-01-02", s.created[0].DueDate)
	if err != nil {
		t.Fatalf("unparseable due date %q: %v", s.created[0].DueDate, err)
	}
	if due.Before(request.ExpiresAt.Truncate(24 * time.Hour)) {
		t.Fatalf("due date %s precedes the hold expiry %s: the buyer would get a code that dies before their reservation",
			due, request.ExpiresAt)
	}
}
