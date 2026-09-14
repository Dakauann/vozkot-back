package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	domain "vozkot/domain/idempotency"
)

// IdempotencyHeader is the header clients send. The name is the one every
// payment API uses, so an SDK or an HTTP client's retry middleware already
// knows it.
const IdempotencyHeader = "Idempotency-Key"

// maxIdempotentBody caps what will be buffered to hash a request.
const maxIdempotentBody = 1 << 20

// Idempotency enforces the replay contract on unsafe requests.
//
// The rules, in the order they are checked:
//
//  1. No key on an endpoint that requires one is a client error, not a free
//     pass. A payment endpoint that silently accepts unkeyed requests is a
//     payment endpoint that double-charges the first time a phone loses signal.
//  2. A key seen before with the SAME body replays the stored response, without
//     running anything.
//  3. A key seen before with a DIFFERENT body is refused. Replaying the first
//     response would hide a client bug behind a success.
//  4. A key whose first request is still running is refused with 409, because
//     the honest answer is "ask again in a moment", not a second checkout.
//  5. A key whose first request DIED, because the process was killed or a
//     deploy cut it, is taken over once its lease lapses, rather than
//     answering "still in progress" for the next day. The work may already
//     have committed, so the endpoint gets one chance to find its own result
//     and replay that instead of doing it twice.
type Idempotency struct {
	store domain.Store
	// status translates a use-case error into a status code. It is injected
	// rather than global so the delivery package that owns those errors owns
	// their mapping too.
	status StatusMapper
	// code names the failure for a client, when the endpoint can. Optional: a
	// nil mapper simply means no codes, and the messages still go out.
	code CodeMapper
	// recovery finds work a previous attempt committed but never recorded.
	// Optional: an endpoint whose work leaves no findable trace has none.
	recovery Recovery
	lease    time.Duration
	now      func() time.Time
}

// StatusMapper translates an error from the handler into an HTTP status.
type StatusMapper func(err error) int

// CodeMapper names an error for a client that has to branch on it.
type CodeMapper func(err error) string

// Recovery answers "did an earlier attempt at this key already commit?".
//
// It exists because the claim and the work are two writes: the key is claimed
// first, the work runs, the response is recorded last. A process that dies in
// the middle leaves a committed order behind a key that looks unstarted. Only
// the endpoint knows how to look, for checkout it is the unique idempotency
// key on the orders table, so the contract lives here and the lookup lives
// there.
//
// Reporting false means "no trace of it", and the handler runs normally.
type Recovery func(ctx context.Context, key string) (result Result, found bool, err error)

// Option configures the replay contract.
type Option func(*Idempotency)

// WithRecovery supplies the lookup that turns an orphaned claim into a replay
// instead of a second checkout.
func WithRecovery(recovery Recovery) Option {
	return func(i *Idempotency) { i.recovery = recovery }
}

// WithCodes names failures for a client that has to branch on them.
func WithCodes(code CodeMapper) Option {
	return func(i *Idempotency) { i.code = code }
}

// WithLease sets how long a claimed request may stay unfinished before a retry
// may take it over. It must exceed the slowest honest request.
func WithLease(lease time.Duration) Option {
	return func(i *Idempotency) {
		if lease > 0 {
			i.lease = lease
		}
	}
}

func NewIdempotency(store domain.Store, status StatusMapper, opts ...Option) *Idempotency {
	if status == nil {
		status = func(error) int { return http.StatusInternalServerError }
	}
	replay := &Idempotency{store: store, status: status, lease: domain.DefaultLease, now: time.Now}
	for _, opt := range opts {
		opt(replay)
	}
	return replay
}

// Result is what a handler produces: the status and the value to encode.
type Result struct {
	Status int
	Body   any
}

// Handler does the actual work once, given the decoded body bytes.
type Handler func(ctx context.Context, body []byte) (Result, error)

var (
	ErrKeyRequired = errors.New("the " + IdempotencyHeader + " header is required for this request")
	ErrKeyReused   = domain.ErrRequestMismatch
	ErrInFlight    = domain.ErrInFlight
)

// Execute runs handler under the key in the request, replaying when it can.
//
// It writes the response itself, because a replay has to reproduce the original
// status code as well as the original body.
func (i *Idempotency) Execute(response http.ResponseWriter, request *http.Request, scope string, handler Handler) {
	key := strings.TrimSpace(request.Header.Get(IdempotencyHeader))
	if key == "" {
		WriteError(response, http.StatusBadRequest, ErrKeyRequired)
		return
	}
	if len(key) > 255 {
		WriteError(response, http.StatusBadRequest, errors.New(IdempotencyHeader+" must be at most 255 characters"))
		return
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, maxIdempotentBody))
	if err != nil {
		WriteError(response, http.StatusBadRequest, errors.New("request body could not be read"))
		return
	}
	hash := domain.HashRequest(body)

	claim, err := i.store.Begin(request.Context(), key, scope, hash, i.lease, i.now())
	if err != nil {
		WriteError(response, http.StatusInternalServerError, err)
		return
	}

	if !claim.Mine {
		existing := claim.Existing
		switch { //nolint:staticcheck // the nil branch is the unreachable-by-contract guard
		case existing == nil:
			WriteError(response, http.StatusInternalServerError, errors.New("idempotency claim is in an unknown state"))
		case existing.RequestHash != hash:
			WriteError(response, http.StatusConflict, ErrKeyReused)
		case existing.State == domain.StateCompleted:
			// The replay. Byte for byte what the first request answered.
			i.writeReplay(response, existing.StatusCode, existing.Response)
		default:
			// Still running, and its lease has not lapsed. Retry-After tells a
			// well-behaved client how long to wait instead of hammering.
			response.Header().Set("Retry-After", "2")
			WriteError(response, http.StatusConflict, ErrInFlight)
		}
		return
	}

	// The claim was taken over from a request that never came back. Before
	// doing the work a second time, look for the work the first one may already
	// have committed: the order exists, only the record of the response was
	// lost. Doing the checkout again instead would reserve a second batch of
	// tickets for a buyer who is looking at the first.
	if claim.Recovered && i.recovery != nil {
		recovered, found, err := i.recovery(request.Context(), key)
		if err != nil {
			i.release(request.Context(), key, scope)
			WriteCodedError(response, i.status(err), i.codeFor(err), err)
			return
		}
		if found {
			encoded, err := json.Marshal(recovered.Body)
			if err != nil {
				i.release(request.Context(), key, scope)
				WriteError(response, http.StatusInternalServerError, err)
				return
			}
			// Recorded as well as returned, so every later retry is an ordinary
			// replay rather than another recovery.
			if err := i.store.Complete(request.Context(), key, scope, recovered.Status, encoded, i.now()); err != nil {
				_ = err
			}
			i.writeReplay(response, recovered.Status, encoded)
			return
		}
	}

	result, err := handler(request.Context(), body)
	if err != nil {
		// The claim is dropped so the client may genuinely retry. Keeping it
		// would answer every retry for the next day with "still in progress"
		// for work that already failed.
		i.release(request.Context(), key, scope)
		WriteCodedError(response, i.status(err), i.codeFor(err), err)
		return
	}

	encoded, err := json.Marshal(result.Body)
	if err != nil {
		WriteError(response, http.StatusInternalServerError, err)
		return
	}
	if err := i.store.Complete(request.Context(), key, scope, result.Status, encoded, i.now()); err != nil {
		// The work succeeded; failing to record the key only costs idempotency
		// on a later retry, so the successful response is still returned.
		_ = err
	}

	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(result.Status)
	_, _ = response.Write(encoded)
}

// writeReplay answers with what an earlier attempt produced, marked so a client
// can tell a replay from a fresh execution.
func (i *Idempotency) writeReplay(response http.ResponseWriter, status int, body []byte) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Idempotent-Replay", "true")
	if status == 0 {
		status = http.StatusOK
	}
	response.WriteHeader(status)
	if len(body) > 0 {
		_, _ = response.Write(body)
	}
}

// release drops a claim whose work failed. Nothing better can be done when the
// release itself fails: the request has already failed, and the lease means the
// claim is takeable again shortly regardless.
func (i *Idempotency) release(ctx context.Context, key, scope string) {
	if err := i.store.Release(ctx, key, scope); err != nil {
		_ = err
	}
}

// codeFor names an error when a mapper was supplied, and says nothing when one
// was not.
func (i *Idempotency) codeFor(err error) string {
	if i.code == nil {
		return ""
	}
	return i.code(err)
}
