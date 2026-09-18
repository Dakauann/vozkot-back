package asaas

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	queuedomain "vozkot/domain/queue"
)

// A 429 has to carry the provider's own pacing all the way to the job ledger.
//
// Asaas caps an account at 25,000 requests per twelve hours and answers a
// breach with 429 plus RateLimit-Reset, the seconds left in the window.
// Retrying before that is refused again AND spends quota doing it, so the
// header is the difference between waiting once and waiting many times.
func TestARateLimitCarriesItsPacingToTheQueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("RateLimit-Reset", "45")
		response.WriteHeader(http.StatusTooManyRequests)
		_, _ = response.Write([]byte(`{"errors":[{"code":"rate_limit","description":"too many requests"}]}`))
	}))
	defer server.Close()

	client := NewClient("key", server.URL)
	err := client.do(context.Background(), http.MethodGet, "/payments", nil, nil)
	if err == nil {
		t.Fatal("a 429 was not reported as an error")
	}

	// The queue must be able to read it without knowing what HTTP is.
	delay, ok := queuedomain.RetryAfter(err)
	if !ok {
		t.Fatal("the 429 reached the queue with no pacing: the backoff curve would " +
			"guess, and guessing short is refused again")
	}
	if delay != 45*time.Second {
		t.Errorf("pacing = %s, want 45s", delay)
	}
	// And it is still classified as worth retrying at all.
	var responseErr *ResponseError
	if !asResponseError(err, &responseErr) || !responseErr.Retryable() {
		t.Error("a 429 was not classified as retryable")
	}
}

func TestTheStandardHeaderIsReadToo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		// What an edge or a proxy in front of the API would send.
		response.Header().Set("Retry-After", "12")
		response.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	err := NewClient("key", server.URL).do(context.Background(), http.MethodGet, "/payments", nil, nil)
	if delay, ok := queuedomain.RetryAfter(err); !ok || delay != 12*time.Second {
		t.Errorf("pacing = %s (ok=%t), want 12s", delay, ok)
	}
}

// Nonsense must leave the curve in charge rather than collapsing to an
// immediate retry, which is the one response that makes a rate limit worse.
func TestAMalformedHeaderIsIgnored(t *testing.T) {
	for _, value := range []string{"", "soon", "-5", "0", "Wed, 21 Oct 2026 07:28:00 GMT"} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			if value != "" {
				response.Header().Set("Retry-After", value)
			}
			response.WriteHeader(http.StatusTooManyRequests)
		}))
		err := NewClient("key", server.URL).do(context.Background(), http.MethodGet, "/payments", nil, nil)
		server.Close()

		if _, ok := queuedomain.RetryAfter(err); ok {
			t.Errorf("Retry-After %q was accepted as a duration", value)
		}
		// The curve still applies, so the job is not retried instantly.
		if delay := queuedomain.RetryDelay(err, 1); delay < queuedomain.Backoff(1) {
			t.Errorf("Retry-After %q produced a %s delay, earlier than the curve", value, delay)
		}
	}
}

// A provider asking for an hour should not hold a job open for an hour; the
// attempt budget should run out and park it for a person.
func TestAnAbsurdHintIsCapped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Retry-After", "86400")
		response.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	err := NewClient("key", server.URL).do(context.Background(), http.MethodGet, "/payments", nil, nil)
	delay, ok := queuedomain.RetryAfter(err)
	if !ok {
		t.Fatal("the hint was dropped entirely")
	}
	if delay > 10*time.Minute {
		t.Errorf("pacing = %s, want it capped to something a buyer could wait out", delay)
	}
}

func asResponseError(err error, target **ResponseError) bool {
	for err != nil {
		if typed, ok := err.(*ResponseError); ok {
			*target = typed
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}
