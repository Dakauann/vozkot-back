package asaas

import (
	"context"
	"strings"
	"testing"
	"time"

	"vozkot/domain/payment"
)

// How many provider requests one order costs.
//
// THIS IS A CAPACITY TEST, not a behaviour one. Asaas enforces per-endpoint
// rolling limits on top of its account quota, measured from live response
// headers on 2026-09-18:
//
//	GET /payments      (list and search)  RateLimit-Limit: 140 per 60s
//	GET /payments/{id} (read one charge)  RateLimit-Limit: 100 per 60s
//	account quota                         25,000 per 12 hours, all endpoints
//
// Every charge this system creates spends one GET /payments on the idempotency
// search, so the 140/min bucket is a hard ceiling of about 2.3 orders per
// second no matter how many replicas are running. That makes the number below
// a capacity figure: adding one call to this path costs real throughput, and
// this test fails when somebody does.
func budget(requests []string, method, path string) int {
	count := 0
	for _, request := range requests {
		if strings.HasPrefix(request, method+" "+path) {
			count++
		}
	}
	return count
}

func TestTheRequestBudgetForOneCharge(t *testing.T) {
	provider := newStub()
	server := provider.server(t)
	gateway := NewGateway(NewClient("key", server.URL))

	_, err := gateway.CreateCharge(context.Background(), payment.ChargeRequest{
		Method:            payment.MethodPix,
		AmountCents:       11_000,
		Currency:          "BRL",
		ExternalReference: "ord_budget_1",
		ExpiresAt:         time.Now().Add(30 * time.Minute),
		Customer: payment.Customer{
			Name: "Maria Souza", Email: "maria@vozkot.test", Document: "12345678909",
		},
	})
	if err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}

	total := len(provider.requests)
	t.Logf("one charge cost %d provider requests: %v", total, provider.requests)

	// The search is the expensive one: it rides the 140/min bucket, so it is
	// the ceiling on orders per second for the whole system.
	if got := budget(provider.requests, "GET", "/payments?"); got != 1 {
		t.Errorf("idempotency searches = %d, want exactly 1: this call is capped at "+
			"140/min and is what bounds orders per second", got)
	}
	// Four is the shape Asaas forces: no idempotency header (so search first),
	// a required customer record (so look it up), the charge, and the QR as a
	// second call. Mercado Pago does the same work in one request.
	if total > 4 {
		t.Errorf("one charge cost %d requests, want at most 4: every extra call "+
			"divides the achievable orders per second", total)
	}
}

// A buyer Asaas has never seen costs one request MORE, because the customer
// has to be created before the charge can name a payer.
//
// Worth measuring rather than assuming: it is the entire value of caching the
// Asaas customer id on the account, which would take the common path from four
// requests to three and raise the achievable orders per second by a quarter.
func TestAnUnknownBuyerCostsAnExtraRequest(t *testing.T) {
	request := payment.ChargeRequest{
		Method: payment.MethodPix, AmountCents: 11_000, Currency: "BRL",
		ExpiresAt: time.Now().Add(30 * time.Minute),
		Customer: payment.Customer{
			Name: "Maria Souza", Email: "maria@vozkot.test", Document: "12345678909",
		},
	}

	// Never seen: the search comes back empty and the customer is created.
	unknown := newStub()
	unknown.searchEmpty = true
	request.ExternalReference = "ord_budget_2"
	if _, err := NewGateway(NewClient("key", unknown.server(t).URL)).
		CreateCharge(context.Background(), request); err != nil {
		t.Fatalf("CreateCharge() for an unknown buyer: %v", err)
	}

	// Known: the search finds them.
	known := newStub()
	request.ExternalReference = "ord_budget_3"
	if _, err := NewGateway(NewClient("key", known.server(t).URL)).
		CreateCharge(context.Background(), request); err != nil {
		t.Fatalf("CreateCharge() for a known buyer: %v", err)
	}

	t.Logf("unknown buyer: %d requests %v", len(unknown.requests), unknown.requests)
	t.Logf("known buyer:   %d requests %v", len(known.requests), known.requests)

	if budget(unknown.requests, "POST", "/customers") != 1 {
		t.Error("an unknown buyer did not cost a customer creation; the two paths " +
			"are the same and this test proves nothing")
	}
	if len(unknown.requests) != len(known.requests)+1 {
		t.Errorf("unknown = %d requests, known = %d; want exactly one more",
			len(unknown.requests), len(known.requests))
	}
}

// Settlement and the expiry void, which ride the tighter 100/min bucket.
func TestTheRequestBudgetForReadingAndVoiding(t *testing.T) {
	provider := newStub()
	server := provider.server(t)
	gateway := NewGateway(NewClient("key", server.URL))
	ctx := context.Background()

	charge, err := gateway.CreateCharge(ctx, payment.ChargeRequest{
		Method: payment.MethodPix, AmountCents: 11_000, Currency: "BRL",
		ExternalReference: "ord_budget_4", ExpiresAt: time.Now().Add(30 * time.Minute),
		Customer: payment.Customer{Name: "Maria", Email: "m@vozkot.test", Document: "12345678909"},
	})
	if err != nil {
		t.Fatalf("CreateCharge() error = %v", err)
	}

	provider.requests = nil
	if _, err := gateway.GetCharge(ctx, charge.ID); err != nil {
		t.Fatalf("GetCharge() error = %v", err)
	}
	reads := len(provider.requests)
	if reads != 1 {
		t.Errorf("one settlement read cost %d requests, want 1: this is the "+
			"100/min bucket and every webhook and every reconcile spends one", reads)
	}

	provider.requests = nil
	if err := gateway.CancelCharge(ctx, charge.ID); err != nil {
		t.Fatalf("CancelCharge() error = %v", err)
	}
	if voids := len(provider.requests); voids != 1 {
		t.Errorf("one void cost %d requests, want 1", voids)
	}
	t.Logf("settlement read: %d request; expiry void: 1 request", reads)
}
