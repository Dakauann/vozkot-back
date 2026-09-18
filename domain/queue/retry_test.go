package queue

import (
	"errors"
	"testing"
	"time"
)

// What the queue waits before trying again.
//
// TWO THINGS GO WRONG WITHOUT THIS, and both are rate-limit failures. Ignoring
// a provider's Retry-After means retrying while it is still refusing, which on
// most providers extends the penalty and spends quota learning nothing. And a
// deterministic curve means a hundred jobs refused in the same second come back
// in the same second: the herd that tripped the limit trips it again, in step.

type paced struct{ after time.Duration }

func (p paced) Error() string             { return "429 too many requests" }
func (p paced) RetryAfter() time.Duration { return p.after }

func TestTheProvidersOwnPacingWins(t *testing.T) {
	// Attempt 1's curve would be 5s. The provider says 30, and the provider is
	// stating fact where the curve is guessing.
	delay := RetryDelay(paced{after: 30 * time.Second}, 1)

	if delay < 30*time.Second {
		t.Fatalf("delay = %s, want at least the 30s the provider asked for: "+
			"returning early is refused again and extends the limit", delay)
	}
	if delay > 45*time.Second {
		t.Errorf("delay = %s, want the hint plus a bounded spread, not an open-ended wait", delay)
	}
}

// A Retry-After must never be read as permission to retry sooner, even when
// the curve has climbed past it.
func TestPacingIsAFloorNotACeiling(t *testing.T) {
	for attempt := 1; attempt <= 8; attempt++ {
		if delay := RetryDelay(paced{after: 20 * time.Second}, attempt); delay < 20*time.Second {
			t.Fatalf("attempt %d waited %s, less than the 20s asked for", attempt, delay)
		}
	}
}

// Nothing said: the curve decides, and is still jittered.
func TestWithoutAHintTheCurveDecides(t *testing.T) {
	plain := errors.New("connection reset")
	for attempt := 1; attempt <= 8; attempt++ {
		base := Backoff(attempt)
		delay := RetryDelay(plain, attempt)
		if delay < base {
			t.Errorf("attempt %d waited %s, earlier than the curve's %s", attempt, delay, base)
		}
		// The spread is capped so a five-minute backoff cannot become eight.
		if delay > base+30*time.Second+time.Second {
			t.Errorf("attempt %d waited %s, well past the curve's %s", attempt, delay, base)
		}
	}
}

// The herd test. A burst refused together must not return together.
func TestABurstDoesNotRetryInLockstep(t *testing.T) {
	const burst = 200
	seen := map[time.Duration]int{}
	for i := 0; i < burst; i++ {
		seen[RetryDelay(paced{after: 30 * time.Second}, 1)]++
	}
	// Without jitter this is exactly 1: every job returns at the same instant.
	if len(seen) < burst/10 {
		t.Fatalf("%d jobs produced only %d distinct delays: they return in lockstep and "+
			"trip the limit again together", burst, len(seen))
	}
	// And no single instant may carry most of the burst.
	for delay, count := range seen {
		if count > burst/4 {
			t.Errorf("%d of %d jobs all retry at %s", count, burst, delay)
		}
	}
}

// A malformed or absent hint must leave the curve in charge rather than
// collapsing the delay to zero, which would be an immediate hot retry.
func TestAZeroHintIsIgnored(t *testing.T) {
	if _, ok := RetryAfter(paced{after: 0}); ok {
		t.Error("a zero Retry-After was treated as a hint")
	}
	if delay := RetryDelay(paced{after: 0}, 1); delay < Backoff(1) {
		t.Errorf("delay = %s, want at least the curve's %s", delay, Backoff(1))
	}
}
