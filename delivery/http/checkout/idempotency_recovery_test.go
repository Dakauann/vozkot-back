package checkout

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	idempotencydomain "vozkot/domain/idempotency"
	"vozkot/infra/testsupport"
)

// requestHash is what the endpoint stores against a key, so a seeded claim
// looks exactly like one a real request left behind.
func requestHash(body string) string { return idempotencydomain.HashRequest([]byte(body)) }

// A claim is written before the work and completed after it, so a process that
// dies in between leaves a real order behind a key that looks unstarted.
//
// Before the lease, that buyer was told "a request with this key is still in
// progress" for the next twenty-four hours. They could not see the order they
// had just paid for, and a fresh key would open a SECOND hold on the same
// tickets: the exact double-reservation the key exists to prevent, caused by
// the key. With the graceful-shutdown bug, this happened on every deploy.

// orphan simulates a process killed mid-checkout: the claim stays in
// `processing` with a lease that has run out, and whatever the request had
// already committed stays committed.
func (h *harness) orphan(t *testing.T, key string) {
	t.Helper()
	err := h.db.Exec(`
		UPDATE idempotency_keys
		SET state = 'processing', status_code = 0, response = NULL, completed_at = NULL,
		    lease_expires_at = NOW() - INTERVAL '1 minute'
		WHERE key = ? AND scope = 'checkout'`, key).Error
	if err != nil {
		t.Fatalf("orphan the claim: %v", err)
	}
}

func (h *harness) orderIDFrom(t *testing.T, body []byte) string {
	t.Helper()
	var envelope OrderEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode order: %v (body %s)", err, body)
	}
	return envelope.Data.ID
}

// TestARetryAfterACrashSeesTheOrderItAlreadyPlaced is the whole point of the
// recovery hook.
func TestARetryAfterACrashSeesTheOrderItAlreadyPlaced(t *testing.T) {
	h := newHarness(t, 10)
	key := testsupport.Unique("key")

	first := h.post(t, key, h.body(2))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	originalID := h.orderIDFrom(t, first.Body.Bytes())

	// The process died after the checkout committed and before the response was
	// recorded.
	h.orphan(t, key)

	second := h.post(t, key, h.body(2))

	if second.Code != http.StatusCreated {
		t.Fatalf("retry status = %d, want the original 201; body %s", second.Code, second.Body.String())
	}
	if got := h.orderIDFrom(t, second.Body.Bytes()); got != originalID {
		t.Fatalf("retry returned order %s, want the one already placed (%s)", got, originalID)
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatal("the recovered response was not marked as a replay")
	}
	if got := h.reserved(t); got != 2 {
		t.Fatalf("reserved = %d, want 2: the retry reserved a second batch of the buyer's own tickets", got)
	}
}

// TestARecoveredResponseIsRecordedForLaterRetries: once recovered, the key
// behaves like any completed key, so a third attempt is an ordinary replay
// rather than another lookup.
func TestARecoveredResponseIsRecordedForLaterRetries(t *testing.T) {
	h := newHarness(t, 10)
	key := testsupport.Unique("key")
	if response := h.post(t, key, h.body(1)); response.Code != http.StatusCreated {
		t.Fatalf("first status = %d", response.Code)
	}
	h.orphan(t, key)
	if response := h.post(t, key, h.body(1)); response.Code != http.StatusCreated {
		t.Fatalf("recovery status = %d", response.Code)
	}

	var state string
	h.db.Raw("SELECT state FROM idempotency_keys WHERE key = ? AND scope = 'checkout'", key).Scan(&state)

	if state != "completed" {
		t.Fatalf("key state = %q, want completed after a recovery", state)
	}
	if got := h.reserved(t); got != 1 {
		t.Fatalf("reserved = %d, want 1", got)
	}
}

// TestACrashBeforeTheTransactionLetsTheRetryThrough: the other half. Nothing
// committed, so there is nothing to recover and the retry must simply work.
func TestACrashBeforeTheTransactionLetsTheRetryThrough(t *testing.T) {
	h := newHarness(t, 10)
	key := testsupport.Unique("key")

	// A claim with no order behind it: the process died between claiming the
	// key and committing the checkout.
	err := h.db.Exec(`
		INSERT INTO idempotency_keys (key, scope, request_hash, state, status_code, created_at, lease_expires_at, expires_at)
		VALUES (?, 'checkout', ?, 'processing', 0, NOW(), NOW() - INTERVAL '1 minute', NOW() + INTERVAL '1 day')`,
		key, requestHash(h.body(2))).Error
	if err != nil {
		t.Fatalf("seed the orphaned claim: %v", err)
	}

	response := h.post(t, key, h.body(2))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: an orphaned claim with no work behind it must not block the retry; body %s",
			response.Code, response.Body.String())
	}
	if got := h.reserved(t); got != 2 {
		t.Fatalf("reserved = %d, want 2", got)
	}
}

// TestALiveClaimIsStillRefused: the lease only applies once it has lapsed. A
// request that is genuinely still running must keep getting 409, or the lease
// would become a licence to run two checkouts at once.
func TestALiveClaimIsStillRefused(t *testing.T) {
	h := newHarness(t, 10)
	key := testsupport.Unique("key")
	err := h.db.Exec(`
		INSERT INTO idempotency_keys (key, scope, request_hash, state, status_code, created_at, lease_expires_at, expires_at)
		VALUES (?, 'checkout', ?, 'processing', 0, NOW(), NOW() + INTERVAL '1 minute', NOW() + INTERVAL '1 day')`,
		key, requestHash(h.body(1))).Error
	if err != nil {
		t.Fatalf("seed the live claim: %v", err)
	}

	response := h.post(t, key, h.body(1))

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while the first request is still running", response.Code)
	}
	if got := h.reserved(t); got != 0 {
		t.Fatalf("reserved = %d, want 0: a refused retry must hold nothing", got)
	}
}

// TestATakeoverRefusesADifferentBody: an orphaned claim is still a claim on
// that key. A different body under it is a client bug, and answering it with
// someone else's order would hide that behind a success.
func TestATakeoverRefusesADifferentBody(t *testing.T) {
	h := newHarness(t, 10)
	key := testsupport.Unique("key")
	if response := h.post(t, key, h.body(1)); response.Code != http.StatusCreated {
		t.Fatalf("first status = %d", response.Code)
	}
	h.orphan(t, key)

	response := h.post(t, key, h.body(3))

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a key reused with a different body", response.Code)
	}
	if got := h.reserved(t); got != 1 {
		t.Fatalf("reserved = %d, want 1", got)
	}
}

// TestConcurrentTakeoversProduceOneOwner: the takeover is one conditional
// UPDATE, so several retries of an orphaned request produce one owner and no
// second checkout.
func TestConcurrentTakeoversProduceOneOwner(t *testing.T) {
	h := newHarness(t, 20)
	key := testsupport.Unique("key")
	if response := h.post(t, key, h.body(2)); response.Code != http.StatusCreated {
		t.Fatalf("first status = %d", response.Code)
	}
	h.orphan(t, key)

	var wait sync.WaitGroup
	bodies := make([]string, 6)
	codes := make([]int, 6)
	for index := range codes {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			response := h.post(t, key, h.body(2))
			codes[index] = response.Code
			bodies[index] = response.Body.String()
		}(index)
	}
	wait.Wait()

	for index, code := range codes {
		switch code {
		case http.StatusCreated, http.StatusConflict:
		default:
			t.Fatalf("attempt %d got %d: %s", index, code, bodies[index])
		}
	}
	if got := h.reserved(t); got != 2 {
		t.Fatalf("reserved = %d, want exactly one batch of 2 across six concurrent takeovers", got)
	}
}
