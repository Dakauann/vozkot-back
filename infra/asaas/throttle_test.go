package asaas_test

// The adapter against a provider that refuses.
//
// `package asaas_test` rather than `package asaas`, because asaastest imports
// asaas and an internal test file importing it back would be a cycle. The
// external test package is the sanctioned way round that, and it also means
// this exercises only the exported surface, which is what the load harness and
// the payment use case see.

import (
	"context"
	"errors"
	"testing"

	"vozkot/domain/payment"
	"vozkot/infra/asaas"
	"vozkot/infra/asaas/asaastest"
)

func chargeFor(reference string) payment.ChargeRequest {
	return payment.ChargeRequest{
		Method:            payment.MethodPix,
		AmountCents:       10_890,
		Currency:          "BRL",
		Description:       "Ingresso",
		ExternalReference: reference,
		IdempotencyKey:    reference,
		Customer: payment.Customer{
			Name:     "Maria Silva",
			Email:    "maria@example.com",
			Document: "12345678909",
		},
	}
}

// The property the whole pacing design rests on: a rate limit does not arrive
// as an opaque failure, it arrives carrying how long to wait, and the error the
// queue receives can answer that question.
//
// Without this the job ledger falls back to its own exponential curve, which
// either retries too early and spends quota being refused, or waits far longer
// than the provider asked and turns a one minute bucket into ten minutes of
// buyers staring at a spinner.
func TestARateLimitReachesTheCallerAsPacing(t *testing.T) {
	provider := asaastest.New()
	server := provider.Server()
	defer server.Close()
	gateway := asaas.NewGateway(asaas.NewClient("key", server.URL))

	// Fill the search bucket. Every charge spends one GET /payments on its
	// idempotency lookup, so this is what an on-sale does on its own.
	var refusal error
	attempts := 0
	for index := 0; index < asaastest.SearchLimit+5; index++ {
		attempts++
		_, err := gateway.CreateCharge(context.Background(), chargeFor("ord_throttle"))
		if err != nil {
			refusal = err
			break
		}
	}
	if refusal == nil {
		t.Fatalf("after %d charges the provider never refused; the limits are not reaching the adapter", attempts)
	}

	// Retryable, or the queue would park a job that is merely early.
	if !payment.Retryable(refusal) {
		t.Errorf("a rate limit was classified as permanent: %v", refusal)
	}

	// And it carries the wait, which is the part a guessing backoff cannot do.
	var response *asaas.ResponseError
	if !errors.As(refusal, &response) {
		t.Fatalf("refusal was %T, want *asaas.ResponseError: %v", refusal, refusal)
	}
	if response.RetryAfter() <= 0 {
		t.Errorf("RetryAfter() = %v, want the window the provider asked for", response.RetryAfter())
	}
	t.Logf("refused after %d charges, asked to wait %v", attempts, response.RetryAfter())
}

// How many provider requests one charge costs, measured through the real
// adapter rather than asserted.
//
// This is the number that sets the ceiling: at one GET /payments per charge
// against 140 per minute, no amount of horizontal scaling gets past about 2.3
// orders a second. It is here as well as in budget_test.go because this file
// sees only the exported surface, which is the one a capacity plan is built on.
func TestOneChargeSpendsExactlyOneIdempotencySearch(t *testing.T) {
	provider := asaastest.Unlimited()
	server := provider.Server()
	defer server.Close()
	gateway := asaas.NewGateway(asaas.NewClient("key", server.URL))

	if _, err := gateway.CreateCharge(context.Background(), chargeFor("ord_budget")); err != nil {
		t.Fatalf("CreateCharge(): %v", err)
	}

	if got := provider.Calls("GET /payments?"); got != 1 {
		t.Errorf("idempotency searches = %d, want exactly 1: this call is what caps orders per second", got)
	}
	total, _, _ := provider.Stats()
	if total > 4 {
		t.Errorf("one charge cost %d provider requests, want at most 4", total)
	}
	t.Logf("one charge cost %d provider requests", total)
}
