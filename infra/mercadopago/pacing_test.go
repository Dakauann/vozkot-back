package mercadopago

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	queuedomain "vozkot/domain/queue"
)

// Mercado Pago publishes no numeric rate limit. It answers 429
// "usage_quota_exceeded" and tells integrators to read Retry-After and back off
// with jitter, so that header is the ONLY reliable information about a ceiling
// that is otherwise unpublished and elastic. Ignoring it means discovering the
// limit by hitting it repeatedly.
func TestAQuotaRefusalCarriesItsPacingToTheQueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Retry-After", "30")
		response.WriteHeader(http.StatusTooManyRequests)
		_, _ = response.Write([]byte(`{"message":"Too many requests","error":"usage_quota_exceeded"}`))
	}))
	defer server.Close()

	client := NewClient("TEST-token", server.URL)
	_, err := client.GetPayment(context.Background(), "123456")
	if err == nil {
		t.Fatal("a 429 was not reported as an error")
	}

	delay, ok := queuedomain.RetryAfter(err)
	if !ok {
		t.Fatal("the 429 reached the queue with no pacing")
	}
	if delay != 30*time.Second {
		t.Errorf("pacing = %s, want 30s", delay)
	}
	if !Retryable(err) {
		t.Error("a 429 was not classified as retryable")
	}
}

func TestAMalformedRetryAfterLeavesTheCurveInCharge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Retry-After", "later")
		response.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := NewClient("TEST-token", server.URL).GetPayment(context.Background(), "123456")
	if _, ok := queuedomain.RetryAfter(err); ok {
		t.Error("an unparseable Retry-After was accepted")
	}
	if delay := queuedomain.RetryDelay(err, 1); delay < queuedomain.Backoff(1) {
		t.Errorf("delay = %s, earlier than the curve: a hot retry makes a limit worse", delay)
	}
}
