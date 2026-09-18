package mercadopago

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultBaseURL is Mercado Pago's single global API host. There is no separate
// sandbox host: test versus production is decided by which access token is
// used, and the resulting payment carries live_mode=false.
const DefaultBaseURL = "https://api.mercadopago.com"

const defaultTimeout = 30 * time.Second

const defaultMaxConnectionsPerHost = 256

// ExpirationLayout is the only layout Mercado Pago accepts for
// date_of_expiration: ISO-8601 with milliseconds and an explicit offset.
const ExpirationLayout = "2006-01-02T15:04:05.000-07:00"

// PIX expiry bounds enforced by Mercado Pago. A request outside them is
// rejected, so the client clamps rather than letting a charge fail outright.
const (
	MinPixExpiry = 30 * time.Minute
	MaxPixExpiry = 30 * 24 * time.Hour
)

// paymentIDPattern guards path interpolation. Mercado Pago payment ids are
// numeric, so anything else is a bug or an injection attempt and never reaches
// the network.
var paymentIDPattern = regexp.MustCompile(`^[0-9]+$`)

// Client is the Mercado Pago Payments API surface this integration needs.
type Client interface {
	CreatePayment(ctx context.Context, request CreatePaymentRequest, idempotencyKey string) (*Payment, error)
	GetPayment(ctx context.Context, paymentID string) (*Payment, error)
	RefundPayment(ctx context.Context, paymentID string, amount float64, idempotencyKey string) (*Refund, error)
	CancelPayment(ctx context.Context, paymentID string) (*Payment, error)
}

// HTTPClient is the http.Client subset used, so a test can inject a transport.
type HTTPClient interface {
	Do(request *http.Request) (*http.Response, error)
}

type client struct {
	accessToken     string
	baseURL         string
	notificationURL string
	http            HTTPClient
}

type Option func(*client)

// WithHTTPClient overrides the transport (tests, proxies, custom timeouts).
func WithHTTPClient(transport HTTPClient) Option {
	return func(c *client) {
		if transport != nil {
			c.http = transport
		}
	}
}

// WithNotificationURL sets the per-payment notification_url.
func WithNotificationURL(url string) Option {
	return func(c *client) { c.notificationURL = strings.TrimSpace(url) }
}

func NewClient(accessToken, baseURL string, opts ...Option) Client {
	c := &client{
		accessToken: strings.TrimSpace(accessToken),
		baseURL:     strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:        defaultHTTPClient(),
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func defaultHTTPClient() *http.Client {
	// net/http's default keeps only two idle connections per host. Payment
	// workers deliberately run many I/O-bound requests concurrently; without a
	// matching keep-alive pool, sustained settlement churns TCP connections and
	// can exhaust the process or NAT gateway's ephemeral ports.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = defaultMaxConnectionsPerHost
	transport.MaxIdleConnsPerHost = defaultMaxConnectionsPerHost
	transport.MaxConnsPerHost = defaultMaxConnectionsPerHost
	return &http.Client{Transport: transport, Timeout: defaultTimeout}
}

var (
	ErrNotFound           = errors.New("mercadopago: resource not found")
	ErrUnauthorized       = errors.New("mercadopago: unauthorized")
	ErrInvalidPaymentID   = errors.New("mercadopago: invalid payment id")
	ErrMissingAccessToken = errors.New("mercadopago: access token is not configured")
)

// ResponseError carries a non-2xx API response, with the parsed envelope when
// Mercado Pago returned one.
type ResponseError struct {
	StatusCode int
	Message    string
	ErrorCode  string
	Causes     []ErrorCause
	Body       string
	// retryAfter is what the provider asked us to wait. Zero when it said
	// nothing.
	retryAfter time.Duration
}

// RetryAfter satisfies queue.Paced, so the job ledger waits as long as Mercado
// Pago asked instead of guessing with its own curve.
//
// Mercado Pago publishes no numeric rate limit; it answers 429
// "usage_quota_exceeded" and tells integrators to read Retry-After and back off
// with jitter. Since the ceiling is unpublished and elastic, the header is the
// ONLY reliable information about it, and ignoring it means discovering the
// limit by repeatedly hitting it.
func (e *ResponseError) RetryAfter() time.Duration { return e.retryAfter }

// retryAfterFrom reads the pacing header, in seconds.
//
// An unparseable value is treated as absent, so a malformed header leaves the
// backoff curve in charge rather than retrying immediately. The cap keeps a
// provider asking for an hour from holding a job open that long; the attempt
// budget should run out and park it for a person instead.
func retryAfterFrom(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0
	}
	if seconds > 600 {
		seconds = 600
	}
	return time.Duration(seconds) * time.Second
}

func (e *ResponseError) Error() string {
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = strings.TrimSpace(e.Body)
	}
	// The cause list is where the actionable detail lives; the message alone is
	// often a generic wrapper. Folding the causes in is what makes a failed
	// charge diagnosable from a log line.
	if detail := formatCauses(e.Causes); detail != "" {
		message += " [" + detail + "]"
	}
	if hint := e.Hint(); hint != "" {
		message += ", likely cause: " + hint
	}
	return fmt.Sprintf("mercadopago: request failed with status %d: %s", e.StatusCode, message)
}

func formatCauses(causes []ErrorCause) string {
	parts := make([]string, 0, len(causes))
	for _, cause := range causes {
		switch {
		case cause.Code != nil && cause.Description != "":
			parts = append(parts, fmt.Sprintf("%v: %s", cause.Code, cause.Description))
		case cause.Description != "":
			parts = append(parts, cause.Description)
		case cause.Code != nil:
			parts = append(parts, fmt.Sprintf("%v", cause.Code))
		}
	}
	return strings.Join(parts, "; ")
}

// knownFailureHints map an opaque Mercado Pago failure onto something an
// operator can act on.
var knownFailureHints = []struct {
	match string
	hint  string
}{
	{"without key enabled for qr render",
		"the collector Mercado Pago account has no PIX key registered; add one before issuing PIX charges"},
	{"collector user without key",
		"the collector Mercado Pago account has no PIX key registered; add one before issuing PIX charges"},
	{"cannot operate between different countries",
		"the access token's country does not match the charge; use a Brazilian (MLB) account"},
	{"payer and collector",
		"the payer email belongs to the same account as the access token; Mercado Pago refuses to let an account pay itself"},
	{"cannot pay yourself",
		"the payer email belongs to the same account as the access token; use a different payer email"},
	{"invalid users involved",
		"payer and collector belong to the same account or to mismatched environments; a TEST- token needs a test-user payer"},
	{"communication_error",
		"Mercado Pago wrapped a downstream rejection: usually no PIX key on the collector account, a payer email that belongs to the collector, or a TEST- token paired with a real payer"},
}

func (e *ResponseError) Hint() string {
	haystack := strings.ToLower(e.Message + " " + e.Body + " " + formatCauses(e.Causes))
	for _, candidate := range knownFailureHints {
		if strings.Contains(haystack, candidate.match) {
			return candidate.hint
		}
	}
	return ""
}

// Unwrap maps transport status onto the package sentinels so callers can use
// errors.Is without inspecting status codes.
func (e *ResponseError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	}
	return nil
}

// Retryable reports whether repeating the request could plausibly succeed. 429
// and 5xx are transient; Mercado Pago documents 423 and 424 as retryable too.
//
// The queue reads this: a rejected card must not be retried eight times, while
// a 502 from the provider should be.
func (e *ResponseError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests, http.StatusLocked, http.StatusFailedDependency:
		return true
	}
	return e.StatusCode >= 500
}

// Retryable reports whether an error from this package is worth another
// attempt. A transport error (no response at all) always is.
func Retryable(err error) bool {
	var responseErr *ResponseError
	if errors.As(err, &responseErr) {
		return responseErr.Retryable()
	}
	return err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrUnauthorized) &&
		!errors.Is(err, ErrInvalidPaymentID) && !errors.Is(err, ErrMissingAccessToken)
}

func validatePaymentID(id string) error {
	if !paymentIDPattern.MatchString(strings.TrimSpace(id)) {
		return fmt.Errorf("%w: %q", ErrInvalidPaymentID, id)
	}
	return nil
}

// NormalizeIdempotencyKey makes any caller-supplied key safe as a header value,
// and invents one when there is none: Mercado Pago requires the header on
// payment writes.
func NormalizeIdempotencyKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return uuid.NewString()
	}
	var builder strings.Builder
	for _, char := range key {
		if char > 32 && char < 127 {
			builder.WriteRune(char)
		}
	}
	normalized := builder.String()
	if normalized == "" {
		return uuid.NewString()
	}
	if len(normalized) > 255 {
		normalized = normalized[:255]
	}
	return normalized
}

func (c *client) do(ctx context.Context, method, path string, body any, idempotencyKey string, out any) error {
	if c.accessToken == "" {
		return ErrMissingAccessToken
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mercadopago: encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("mercadopago: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.accessToken)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// Mercado Pago requires X-Idempotency-Key on payment writes; sending one on
	// every write is what makes a queue redelivery safe to repeat.
	if idempotencyKey != "" {
		request.Header.Set("X-Idempotency-Key", idempotencyKey)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("mercadopago: %s %s: %w", method, path, err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("mercadopago: read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return newResponseError(response.StatusCode, raw, response.Header)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mercadopago: decode response: %w", err)
	}
	return nil
}

func newResponseError(status int, raw []byte, headers http.Header) *ResponseError {
	responseErr := &ResponseError{StatusCode: status, Body: string(raw), retryAfter: retryAfterFrom(headers)}
	var apiErr APIError
	if err := json.Unmarshal(raw, &apiErr); err == nil {
		responseErr.Message = apiErr.Message
		responseErr.ErrorCode = apiErr.Error
		responseErr.Causes = apiErr.Cause
	}
	if responseErr.Message == "" {
		responseErr.Message = strings.TrimSpace(string(raw))
	}
	return responseErr
}

func (c *client) CreatePayment(ctx context.Context, request CreatePaymentRequest, idempotencyKey string) (*Payment, error) {
	if request.NotificationURL == "" {
		request.NotificationURL = c.notificationURL
	}
	var out Payment
	if err := c.do(ctx, http.MethodPost, "/v1/payments", request, NormalizeIdempotencyKey(idempotencyKey), &out); err != nil {
		return nil, err
	}
	if out.ID == 0 {
		return nil, errors.New("mercadopago: create payment returned no id")
	}
	return &out, nil
}

func (c *client) GetPayment(ctx context.Context, paymentID string) (*Payment, error) {
	if err := validatePaymentID(paymentID); err != nil {
		return nil, err
	}
	var out Payment
	if err := c.do(ctx, http.MethodGet, "/v1/payments/"+strings.TrimSpace(paymentID), nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *client) RefundPayment(ctx context.Context, paymentID string, amount float64, idempotencyKey string) (*Refund, error) {
	if err := validatePaymentID(paymentID); err != nil {
		return nil, err
	}
	// A nil body is a FULL refund; an amount makes it partial. The distinction
	// is the presence of the body, not a zero value.
	var body any
	if amount > 0 {
		body = map[string]float64{"amount": amount}
	}
	var out Refund
	if err := c.do(ctx, http.MethodPost, "/v1/payments/"+strings.TrimSpace(paymentID)+"/refunds",
		body, NormalizeIdempotencyKey(idempotencyKey), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *client) CancelPayment(ctx context.Context, paymentID string) (*Payment, error) {
	if err := validatePaymentID(paymentID); err != nil {
		return nil, err
	}
	var out Payment
	body := map[string]string{"status": StatusCancelled}
	if err := c.do(ctx, http.MethodPut, "/v1/payments/"+strings.TrimSpace(paymentID), body, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClampExpiry keeps a requested expiry inside the window Mercado Pago accepts.
func ClampExpiry(expiresAt time.Time, now time.Time) time.Time {
	if expiresAt.IsZero() {
		return now.Add(MinPixExpiry)
	}
	if delta := expiresAt.Sub(now); delta < MinPixExpiry {
		return now.Add(MinPixExpiry)
	} else if delta > MaxPixExpiry {
		return now.Add(MaxPixExpiry)
	}
	return expiresAt
}
