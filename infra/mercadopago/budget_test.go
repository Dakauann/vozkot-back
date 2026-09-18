package mercadopago

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"vozkot/domain/payment"
)

// How many provider requests one order costs on Mercado Pago.
//
// THE COMPARISON THAT MATTERS, measured on both adapters rather than assumed:
// Asaas needs FOUR requests to issue one charge (search by external reference,
// because it has no idempotency header; find or create a customer, because it
// will not bill an anonymous payer; create the payment; fetch the PIX QR),
// while Mercado Pago needs ONE. The payer travels inline, the QR comes back in
// the create response, and idempotency is the X-Idempotency-Key header.
//
// That is a fourfold difference in how much provider budget an order spends,
// independent of any quota, and it is the single biggest reason the same rate
// limit buys very different throughput on the two.
//
// Measured locally against a stub. Mercado Pago publishes no rate limit and,
// verified on 2026-09-18, returns no RateLimit-* headers on a successful
// response, so the only way to learn their ceiling is to trip it. That is not
// something to do to a payments API, which makes OUR call count the number
// worth controlling.
func TestOneChargeCostsOneRequest(t *testing.T) {
	var mu sync.Mutex
	var requests []string

	gateway, _ := newTestGateway(t, func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.Method+" "+request.URL.Path)
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(approvedPixResponse()))
	})

	charge, err := gateway.CreateCharge(context.Background(), payment.ChargeRequest{
		Method:            payment.MethodPix,
		AmountCents:       48_000,
		Currency:          "BRL",
		ExternalReference: "ord_budget_mp",
		ExpiresAt:         time.Now().Add(30 * time.Minute),
		Customer: payment.Customer{
			Name: "Maria Souza", Email: "maria@vozkot.test", Document: "12345678909",
		},
		IdempotencyKey: "ord_budget_mp",
	})
	if err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}

	t.Logf("one charge cost %d provider request(s): %v", len(requests), requests)
	if len(requests) != 1 {
		t.Errorf("one charge cost %d requests, want exactly 1: the payer is inline, "+
			"the QR is in the response and idempotency is a header, so anything more "+
			"is work this provider does not require", len(requests))
	}
	// And the QR really did arrive without a second call, which is what makes
	// one request enough.
	if charge.PixCopyPaste == "" {
		t.Error("the PIX payload was not in the create response; a second call would be needed")
	}
}

// Reading a charge back is one request, and it is what every webhook and every
// reconciliation sweep spends.
func TestOneSettlementReadCostsOneRequest(t *testing.T) {
	var mu sync.Mutex
	count := 0

	gateway, _ := newTestGateway(t, func(response http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(approvedPixResponse()))
	})

	if _, err := gateway.GetCharge(context.Background(), "1234567890"); err != nil {
		t.Fatalf("GetCharge() error = %v", err)
	}
	if count != 1 {
		t.Errorf("one settlement read cost %d requests, want 1", count)
	}
}
