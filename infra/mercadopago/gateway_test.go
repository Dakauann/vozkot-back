package mercadopago

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vozkot/domain/payment"
)

// newTestGateway wires a real client against a stub Mercado Pago.
func newTestGateway(t *testing.T, handler http.HandlerFunc, opts ...GatewayOption) (payment.Gateway, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient("TEST-token", server.URL, WithNotificationURL("https://api.example.com/webhooks/mercadopago"))
	return NewGateway(client, opts...), server
}

func approvedPixResponse() string {
	return `{
		"id": 1234567890,
		"status": "approved",
		"status_detail": "accredited",
		"external_reference": "ord_1",
		"payment_method_id": "pix",
		"transaction_amount": 480.00,
		"point_of_interaction": {"transaction_data": {"qr_code": "00020126-copy-paste", "qr_code_base64": "aGVsbG8="}}
	}`
}

func TestCreateChargeSendsWhatMercadoPagoRequires(t *testing.T) {
	var captured CreatePaymentRequest
	var idempotencyKey string

	gateway, _ := newTestGateway(t, func(response http.ResponseWriter, request *http.Request) {
		idempotencyKey = request.Header.Get("X-Idempotency-Key")
		_ = json.NewDecoder(request.Body).Decode(&captured)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(approvedPixResponse()))
	})

	charge, err := gateway.CreateCharge(context.Background(), payment.ChargeRequest{
		Method:            payment.MethodPix,
		AmountCents:       48000,
		Description:       "Ingresso",
		ExternalReference: "ord_1",
		ExpiresAt:         time.Now().Add(30 * time.Minute),
		Customer:          payment.Customer{Name: "Maria Souza", Email: "maria@exemplo.com.br", Document: "123.456.789-09"},
		IdempotencyKey:    "ord_1",
	})
	if err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}

	// The header is what makes a retried job safe: Mercado Pago returns the
	// original charge instead of creating a second one.
	if idempotencyKey != "ord_1" {
		t.Fatalf("X-Idempotency-Key = %q, want the order id", idempotencyKey)
	}
	// Cents in, decimal on the wire. 48000 centavos is R$ 480,00, not 48000.
	if captured.TransactionAmount != 480 {
		t.Fatalf("transaction_amount = %v, want 480", captured.TransactionAmount)
	}
	if captured.PaymentMethodID != PaymentMethodPix {
		t.Fatalf("payment_method_id = %q", captured.PaymentMethodID)
	}
	if captured.Payer.Identification == nil || captured.Payer.Identification.Number != "12345678909" {
		t.Fatalf("payer identification = %+v, want the CPF digits", captured.Payer.Identification)
	}
	if captured.Payer.Identification.Type != IdentificationCPF {
		t.Fatalf("identification type = %q, want CPF for an 11-digit document", captured.Payer.Identification.Type)
	}
	if captured.Payer.FirstName != "Maria" || captured.Payer.LastName != "Souza" {
		t.Fatalf("payer name = %q %q", captured.Payer.FirstName, captured.Payer.LastName)
	}
	// The layout Mercado Pago accepts, and only that one.
	if _, err := time.Parse(ExpirationLayout, captured.DateOfExpiration); err != nil {
		t.Fatalf("date_of_expiration = %q, which is not the accepted layout: %v", captured.DateOfExpiration, err)
	}
	if captured.NotificationURL == "" {
		t.Fatal("notification_url was not set; the webhook would carry no signed data.id")
	}
	if captured.ExternalReference != "ord_1" {
		t.Fatalf("external_reference = %q, want the order id", captured.ExternalReference)
	}

	if charge.Status != payment.StatusPaid || charge.PixCopyPaste != "00020126-copy-paste" {
		t.Fatalf("charge = %+v", charge)
	}
	if charge.AmountCents != 48000 {
		t.Fatalf("amount = %d cents, want 48000 back in integer cents", charge.AmountCents)
	}
}

func TestCreateChargeRefusesWhatCannotBeCharged(t *testing.T) {
	gateway, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("the provider must not be called for a request that cannot be valid")
	})

	base := payment.ChargeRequest{
		Method:      payment.MethodPix,
		AmountCents: 1000,
		Customer:    payment.Customer{Name: "Maria", Email: "maria@exemplo.com.br", Document: "12345678909"},
	}

	cases := map[string]struct {
		mutate func(*payment.ChargeRequest)
		want   error
	}{
		"no amount":   {func(r *payment.ChargeRequest) { r.AmountCents = 0 }, payment.ErrInvalidAmount},
		"no email":    {func(r *payment.ChargeRequest) { r.Customer.Email = "" }, payment.ErrEmailRequired},
		"no document": {func(r *payment.ChargeRequest) { r.Customer.Document = "" }, payment.ErrDocumentRequired},
		"card":        {func(r *payment.ChargeRequest) { r.Method = payment.MethodCard }, payment.ErrMethodUnsupported},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			request := base
			testCase.mutate(&request)

			if _, err := gateway.CreateCharge(context.Background(), request); !errors.Is(err, testCase.want) {
				t.Fatalf("CreateCharge() error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestGetChargeTranslatesAMissingPayment(t *testing.T) {
	gateway, _ := newTestGateway(t, func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNotFound)
		_, _ = response.Write([]byte(`{"message":"Payment not found","error":"not_found","status":404}`))
	})

	_, err := gateway.GetCharge(context.Background(), "1234567890")

	if !errors.Is(err, payment.ErrChargeNotFound) {
		t.Fatalf("GetCharge() error = %v, want %v", err, payment.ErrChargeNotFound)
	}
}

func TestClientRefusesANonNumericPaymentID(t *testing.T) {
	gateway, _ := newTestGateway(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("a path-traversing id must never reach the network")
	})

	if _, err := gateway.GetCharge(context.Background(), "../../v1/users/me"); err == nil {
		t.Fatal("GetCharge() accepted an id that is not a payment id")
	}
}

func TestResponseErrorsSayWhetherRetryingCouldHelp(t *testing.T) {
	cases := map[int]bool{
		http.StatusBadRequest:         false,
		http.StatusUnauthorized:       false,
		http.StatusNotFound:           false,
		http.StatusTooManyRequests:    true,
		http.StatusBadGateway:         true,
		http.StatusServiceUnavailable: true,
	}

	for status, wantRetryable := range cases {
		t.Run(http.StatusText(status), func(t *testing.T) {
			gateway, _ := newTestGateway(t, func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(status)
				_, _ = response.Write([]byte(`{"message":"nope"}`))
			})

			_, err := gateway.CreateCharge(context.Background(), payment.ChargeRequest{
				Method:      payment.MethodPix,
				AmountCents: 1000,
				Customer:    payment.Customer{Name: "Maria", Email: "maria@exemplo.com.br", Document: "12345678909"},
			})

			if err == nil {
				t.Fatal("CreateCharge() returned no error")
			}
			// The queue reads this: a rejected request must not be retried
			// eight times, while a provider outage should be.
			if got := Retryable(err); got != wantRetryable {
				t.Fatalf("Retryable() = %t, want %t for %d", got, wantRetryable, status)
			}
		})
	}
}

func TestErrorMessageCarriesTheActionableCause(t *testing.T) {
	gateway, _ := newTestGateway(t, func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(`{"message":"fill and validate error list","error":"bad_request","cause":[{"code":13253,"description":"Collector user without key enabled for QR render"}]}`))
	})

	_, err := gateway.CreateCharge(context.Background(), payment.ChargeRequest{
		Method:      payment.MethodPix,
		AmountCents: 1000,
		Customer:    payment.Customer{Name: "Maria", Email: "maria@exemplo.com.br", Document: "12345678909"},
	})

	if err == nil {
		t.Fatal("CreateCharge() returned no error")
	}
	// Without the cause and the hint, this failure reads as "something went
	// wrong" and costs an afternoon.
	if !strings.Contains(err.Error(), "QR render") {
		t.Fatalf("error = %q, want it to carry the provider's cause", err)
	}
	if !strings.Contains(err.Error(), "PIX key") {
		t.Fatalf("error = %q, want it to carry the operator hint", err)
	}
}

func TestSandboxPayerOverrideReplacesTheBuyer(t *testing.T) {
	var captured CreatePaymentRequest
	gateway, _ := newTestGateway(t, func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewDecoder(request.Body).Decode(&captured)
		_, _ = response.Write([]byte(approvedPixResponse()))
	}, WithSandboxPayerEmail("test_user_123@testuser.com"))

	if _, err := gateway.CreateCharge(context.Background(), payment.ChargeRequest{
		Method:      payment.MethodPix,
		AmountCents: 1000,
		Customer:    payment.Customer{Name: "Maria", Email: "maria@exemplo.com.br", Document: "12345678909"},
	}); err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}

	// Sandbox rejects a charge whose payer is not one of its test users, which
	// is the only reason this override exists; configuration refuses it outside
	// development.
	if captured.Payer.Email != "test_user_123@testuser.com" {
		t.Fatalf("payer email = %q, want the sandbox test user", captured.Payer.Email)
	}
}

func TestClampExpiryKeepsMercadoPagosWindow(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

	// Too soon is raised to the minimum: Mercado Pago rejects anything shorter,
	// and a rejected charge helps nobody.
	if got := ClampExpiry(now.Add(time.Minute), now); !got.Equal(now.Add(MinPixExpiry)) {
		t.Fatalf("ClampExpiry(1m) = %v, want %v", got, now.Add(MinPixExpiry))
	}
	// Too far is lowered to the maximum.
	if got := ClampExpiry(now.Add(365*24*time.Hour), now); !got.Equal(now.Add(MaxPixExpiry)) {
		t.Fatalf("ClampExpiry(1y) = %v, want %v", got, now.Add(MaxPixExpiry))
	}
	// Inside the window it is left alone.
	inside := now.Add(2 * time.Hour)
	if got := ClampExpiry(inside, now); !got.Equal(inside) {
		t.Fatalf("ClampExpiry(2h) = %v, want it untouched", got)
	}
}

func TestRefundAndCancelReachTheRightEndpoints(t *testing.T) {
	var paths []string
	gateway, _ := newTestGateway(t, func(response http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.Method+" "+request.URL.Path)
		_, _ = response.Write([]byte(`{"id":1234567890,"status":"refunded","amount":480}`))
	})

	if err := gateway.RefundCharge(context.Background(), "1234567890", 48000); err != nil {
		t.Fatalf("RefundCharge() error = %v", err)
	}
	if err := gateway.CancelCharge(context.Background(), "1234567890"); err != nil {
		t.Fatalf("CancelCharge() error = %v", err)
	}

	want := []string{"POST /v1/payments/1234567890/refunds", "PUT /v1/payments/1234567890"}
	for index, expected := range want {
		if index >= len(paths) || paths[index] != expected {
			t.Fatalf("requests = %v, want %v", paths, want)
		}
	}
}
