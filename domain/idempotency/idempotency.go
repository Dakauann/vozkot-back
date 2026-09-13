// Package idempotency makes an unsafe request safe to repeat.
//
// The contract is the one Stripe established and every payment API has since
// copied: the client sends a key, the server records it with a hash of the
// request and the response it produced, and a retry with the same key returns
// that stored response instead of doing the work again. Without it, a buyer
// whose connection drops mid-checkout taps twice and holds two batches of
// tickets — or pays for both.
package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

type State string

const (
	// StateProcessing is a key whose first request is still running.
	StateProcessing State = "processing"
	// StateCompleted has a response to replay.
	StateCompleted State = "completed"
)

var (
	// ErrInFlight means the original request has not finished. The correct
	// answer is 409 and "try again shortly": returning a fresh execution would
	// be exactly the double-charge the key exists to prevent.
	ErrInFlight = errors.New("a request with this idempotency key is still in progress")
	// ErrRequestMismatch means the key was reused with a different body. That
	// is a client bug, and answering with the first request's response would
	// hide it behind a success.
	ErrRequestMismatch = errors.New("idempotency key was reused with a different request")
	ErrNotFound        = errors.New("idempotency record not found")
)

// Record is one key and what it produced.
type Record struct {
	Key   string
	Scope string
	// RequestHash detects reuse with different parameters.
	RequestHash string
	State       State
	StatusCode  int
	Response    []byte
	CreatedAt   time.Time
	CompletedAt *time.Time
	ExpiresAt   time.Time
}

// TTL is how long a key is honoured. Stripe uses 24 hours and the reasoning
// carries over: long enough to cover any retry a client or a mobile network
// will make, short enough that the table does not grow without bound.
const TTL = 24 * time.Hour

// Store persists keys.
type Store interface {
	// Begin claims a key. It returns the existing record when the key was seen
	// before, and nil when this caller now owns it and must do the work.
	//
	// The claim has to be atomic — one INSERT that either wins or loses —
	// because two simultaneous retries of the same request are the exact race
	// this is protecting against.
	Begin(ctx context.Context, key, scope, requestHash string, now time.Time) (*Record, error)
	// Complete stores the response to replay.
	Complete(ctx context.Context, key, scope string, statusCode int, response []byte, now time.Time) error
	// Release drops a claim whose work failed, so the client can retry rather
	// than being told for 24 hours that a request is still in progress.
	Release(ctx context.Context, key, scope string) error
	// DeleteExpired prunes the table.
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
}

// HashRequest fingerprints a request body. SHA-256 over the exact bytes: the
// comparison only ever needs to answer "same or different".
func HashRequest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
