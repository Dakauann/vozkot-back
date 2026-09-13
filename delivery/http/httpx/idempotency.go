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
type Idempotency struct {
	store domain.Store
	// status translates a use-case error into a status code. It is injected
	// rather than global so the delivery package that owns those errors owns
	// their mapping too.
	status StatusMapper
	now    func() time.Time
}

// StatusMapper translates an error from the handler into an HTTP status.
type StatusMapper func(err error) int

func NewIdempotency(store domain.Store, status StatusMapper) *Idempotency {
	if status == nil {
		status = func(error) int { return http.StatusInternalServerError }
	}
	return &Idempotency{store: store, status: status, now: time.Now}
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

	existing, err := i.store.Begin(request.Context(), key, scope, hash, i.now())
	if err != nil {
		WriteError(response, http.StatusInternalServerError, err)
		return
	}

	if existing != nil {
		switch {
		case existing.RequestHash != hash:
			WriteError(response, http.StatusConflict, ErrKeyReused)
		case existing.State == domain.StateCompleted:
			// The replay. Byte for byte what the first request answered.
			response.Header().Set("Content-Type", "application/json; charset=utf-8")
			response.Header().Set("Idempotent-Replay", "true")
			status := existing.StatusCode
			if status == 0 {
				status = http.StatusOK
			}
			response.WriteHeader(status)
			if len(existing.Response) > 0 {
				_, _ = response.Write(existing.Response)
			}
		default:
			// Still running. Retry-After tells a well-behaved client how long
			// to wait instead of hammering.
			response.Header().Set("Retry-After", "2")
			WriteError(response, http.StatusConflict, ErrInFlight)
		}
		return
	}

	result, err := handler(request.Context(), body)
	if err != nil {
		// The claim is dropped so the client may genuinely retry. Keeping it
		// would answer every retry for the next day with "still in progress"
		// for work that already failed.
		if releaseErr := i.store.Release(request.Context(), key, scope); releaseErr != nil {
			// Nothing better to do than surface it: the request already failed.
			_ = releaseErr
		}
		WriteError(response, i.status(err), err)
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
