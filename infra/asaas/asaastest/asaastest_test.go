package asaastest

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A stub that only ever succeeds cannot show what bounds an on-sale, so these
// are about the refusals.

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	return response.StatusCode, response.Header.Get("RateLimit-Reset")
}

func TestTheSearchBucketRefusesThe141stRequestInAMinute(t *testing.T) {
	provider := New()
	server := provider.Server()
	defer server.Close()

	for index := 1; index <= SearchLimit; index++ {
		if status, _ := get(t, server.URL+"/payments?externalReference=ord_1"); status != http.StatusOK {
			t.Fatalf("request %d of %d got %d, want 200", index, SearchLimit, status)
		}
	}

	status, reset := get(t, server.URL+"/payments?externalReference=ord_1")
	if status != http.StatusTooManyRequests {
		t.Errorf("request %d got %d, want 429", SearchLimit+1, status)
	}
	// The header is what makes the queue wait the right amount instead of
	// guessing with its own curve.
	if reset == "" {
		t.Error("a 429 carried no RateLimit-Reset, so a paced client has nothing to read")
	}
}

// The window slides rather than resetting on a tick, which is what decides
// whether a client that waits exactly as long as it was told gets through.
func TestTheWindowSlides(t *testing.T) {
	clock := time.Now()
	provider := New().WithClock(func() time.Time { return clock })
	server := provider.Server()
	defer server.Close()

	for index := 0; index < SearchLimit; index++ {
		get(t, server.URL+"/payments?externalReference=ord_1")
	}
	if status, _ := get(t, server.URL+"/payments?externalReference=ord_1"); status != http.StatusTooManyRequests {
		t.Fatalf("the bucket did not fill: got %d", status)
	}

	clock = clock.Add(SearchWindow + time.Second)
	if status, _ := get(t, server.URL+"/payments?externalReference=ord_1"); status != http.StatusOK {
		t.Errorf("after the window passed the request got %d, want 200", status)
	}
}

// Reading one charge is a TIGHTER bucket than searching, and payment.sync uses
// it on every webhook, so it is the one an on-sale hits first.
func TestReadingOneChargeHasItsOwnTighterBucket(t *testing.T) {
	provider := New()
	server := provider.Server()
	defer server.Close()

	for index := 1; index <= ReadLimit; index++ {
		status, _ := get(t, server.URL+"/payments/pay_000001")
		// 404 is fine: the charge does not exist, but the request was allowed.
		if status == http.StatusTooManyRequests {
			t.Fatalf("read %d of %d was throttled early", index, ReadLimit)
		}
	}
	if status, _ := get(t, server.URL+"/payments/pay_000001"); status != http.StatusTooManyRequests {
		t.Errorf("read %d got %d, want 429", ReadLimit+1, status)
	}

	// And the search bucket is untouched: they are separate limits.
	if status, _ := get(t, server.URL+"/payments?externalReference=ord_1"); status != http.StatusOK {
		t.Errorf("searching got %d after the READ bucket filled; the buckets are not independent", status)
	}
}

// The escalation the real account performed: a burst of 429s, then a 403 that
// stops everything. This is what happened on 2026-09-18.
func TestPushingThroughRefusalsEarnsA403(t *testing.T) {
	provider := New()
	server := provider.Server()
	defer server.Close()

	for index := 0; index < SearchLimit; index++ {
		get(t, server.URL+"/payments?externalReference=ord_1")
	}
	blocked := false
	for index := 0; index <= BlockAfter+1; index++ {
		status, _ := get(t, server.URL+"/payments?externalReference=ord_1")
		if status == http.StatusForbidden {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Errorf("ignoring %d refusals never produced a 403; the stub cannot reproduce the incident", BlockAfter)
	}

	_, throttled, forbidden := provider.Stats()
	if throttled == 0 || forbidden == 0 {
		t.Errorf("throttled=%d forbidden=%d, want both above zero", throttled, forbidden)
	}
}

// A client that respects the header never reaches the block. This is the
// property the pacing in asaas.ResponseError.RetryAfter exists to provide, and
// the reason a paced system degrades into a slow queue instead of an outage.
func TestAClientThatWaitsIsNeverBlocked(t *testing.T) {
	clock := time.Now()
	provider := New().WithClock(func() time.Time { return clock })
	server := provider.Server()
	defer server.Close()

	for index := 0; index < SearchLimit*3; index++ {
		status, reset := get(t, server.URL+"/payments?externalReference=ord_1")
		if status == http.StatusForbidden {
			t.Fatalf("a client that waits was blocked after %d requests", index)
		}
		if status == http.StatusTooManyRequests {
			// Wait exactly as long as it was told, as the queue does.
			seconds := 1
			if reset != "" {
				_, _ = fmt.Sscanf(reset, "%d", &seconds)
			}
			clock = clock.Add(time.Duration(seconds) * time.Second)
		}
	}
	if _, _, forbidden := provider.Stats(); forbidden != 0 {
		t.Errorf("forbidden=%d, want 0: waiting should never escalate", forbidden)
	}
}

func TestUnlimitedNeverRefuses(t *testing.T) {
	provider := Unlimited()
	server := provider.Server()
	defer server.Close()

	for index := 0; index < SearchLimit+10; index++ {
		if status, _ := get(t, server.URL+"/payments?externalReference=ord_1"); status != http.StatusOK {
			t.Fatalf("request %d got %d from an unlimited provider", index, status)
		}
	}
}
