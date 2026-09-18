package asaas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Production and sandbox hosts. Sandbox is a different host, not a flag, so a
// misconfiguration cannot quietly bill a real card.
const (
	ProductionBaseURL = "https://api.asaas.com/v3"
	SandboxBaseURL    = "https://api-sandbox.asaas.com/v3"
)

const defaultTimeout = 20 * time.Second

var (
	ErrNotFound        = errors.New("asaas: resource not found")
	ErrUnauthorized    = errors.New("asaas: unauthorized")
	ErrMissingAPIKey   = errors.New("asaas: API key is not configured")
	ErrInvalidID       = errors.New("asaas: invalid resource id")
	ErrDocumentMissing = errors.New("asaas: a CPF/CNPJ is required to resolve a customer")
)

// idPattern bounds what may be interpolated into a URL path.
//
// Not paranoia: a charge id arrives from our own database, but it originally
// came from a provider, and one containing "../" would build a request against
// a different endpoint entirely.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ResponseError carries a non-2xx Asaas response.
type ResponseError struct {
	StatusCode int
	Errors     []APIError
	Body       string
	// retryAfter is what the provider asked us to wait, from its own headers.
	// Zero when it said nothing.
	retryAfter time.Duration
}

// RetryAfter satisfies queue.Paced, so the job ledger waits exactly as long as
// Asaas asked rather than guessing with its own curve.
//
// Asaas rate limits on two axes, a 12-hour account quota and per-endpoint
// limits, and answers both with 429 plus RateLimit-Reset, the seconds left in
// the window. Retrying before that is refused again and spends quota doing it.
func (e *ResponseError) RetryAfter() time.Duration { return e.retryAfter }

// retryAfterFrom reads whichever pacing header the provider sent.
//
// RateLimit-Reset is what Asaas documents, as seconds remaining in the window;
// Retry-After is the HTTP standard and is read too, because an edge or a proxy
// in front of the API may be the thing refusing. Both are seconds here. A
// value that cannot be parsed is treated as absent rather than as zero, so a
// malformed header leaves the backoff curve in charge instead of retrying
// immediately.
func retryAfterFrom(headers http.Header) time.Duration {
	if headers == nil {
		return 0
	}
	for _, name := range []string{"Retry-After", "RateLimit-Reset"} {
		raw := strings.TrimSpace(headers.Get(name))
		if raw == "" {
			continue
		}
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			continue
		}
		// A provider asking for an hour is either broken or telling us to stop
		// for the day; either way the queue should park rather than hold a job
		// open that long, so the hint is capped and the attempt budget runs out
		// normally.
		if seconds > 600 {
			seconds = 600
		}
		return time.Duration(seconds) * time.Second
	}
	return 0
}

// APIError is one entry from Asaas's error envelope.
type APIError struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

func (e *ResponseError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, item := range e.Errors {
		switch {
		case item.Code != "" && item.Description != "":
			parts = append(parts, item.Code+": "+item.Description)
		case item.Description != "":
			parts = append(parts, item.Description)
		case item.Code != "":
			parts = append(parts, item.Code)
		}
	}
	detail := strings.Join(parts, "; ")
	if detail == "" {
		detail = strings.TrimSpace(e.Body)
	}
	return fmt.Sprintf("asaas: request failed with status %d: %s", e.StatusCode, detail)
}

// Unwrap maps transport status onto the package sentinels, so callers use
// errors.Is instead of reading status codes.
func (e *ResponseError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	}
	return nil
}

// Retryable reports whether repeating the request could plausibly succeed.
//
// The queue reads this through payment.Retryable: a refused charge must not be
// retried eight times, while a 502 should be. 429 is retryable because Asaas
// rate-limits per account and the next attempt is the whole remedy.
func (e *ResponseError) Retryable() bool {
	if e.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return e.StatusCode >= 500
}

// Client is the raw Asaas HTTP client.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

type Option func(*Client)

// WithHTTPClient replaces the transport, for tests and for a caller that wants
// its own connection pool.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.http = client
		}
	}
}

func NewClient(apiKey, baseURL string, options ...Option) *Client {
	client := &Client{
		apiKey:  strings.TrimSpace(apiKey),
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: defaultTimeout},
	}
	if client.baseURL == "" {
		client.baseURL = ProductionBaseURL
	}
	for _, apply := range options {
		apply(client)
	}
	return client
}

// Configured reports whether this client can actually reach Asaas.
func (c *Client) Configured() bool { return c != nil && c.apiKey != "" }

// Sandbox reports whether charges are play money, for the boot log.
func (c *Client) Sandbox() bool {
	return c != nil && strings.Contains(c.baseURL, "sandbox")
}

// FindCustomerByDocument returns the customer holding this document, or nil.
//
// The empty-document guard is load-bearing rather than defensive: Asaas treats
// `?cpfCnpj=` as a wildcard and answers with SOME customer, so an empty document
// would resolve to an unrelated payer and bill the wrong person.
func (c *Client) FindCustomerByDocument(ctx context.Context, document string) (*Customer, error) {
	document = onlyDigits(document)
	if document == "" {
		return nil, ErrDocumentMissing
	}
	path := "/customers?cpfCnpj=" + url.QueryEscape(document)

	var found list[Customer]
	if err := c.do(ctx, http.MethodGet, path, nil, &found); err != nil {
		return nil, err
	}
	if len(found.Data) == 0 {
		return nil, nil
	}
	return &found.Data[0], nil
}

func (c *Client) CreateCustomer(ctx context.Context, draft Customer) (*Customer, error) {
	draft.Document = onlyDigits(draft.Document)
	if draft.Document == "" {
		return nil, ErrDocumentMissing
	}
	var created Customer
	if err := c.do(ctx, http.MethodPost, "/customers", draft, &created); err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, errors.New("asaas: customer created without an id")
	}
	return &created, nil
}

// FindPaymentByReference looks a charge up by OUR order id.
//
// This is how idempotency is done on a provider that has no idempotency header.
// A charge job that timed out after Asaas had already created the charge would
// otherwise create a second one on retry: two PIX codes, two possible payments,
// one order. Searching on the external reference before creating turns that into
// a lookup.
func (c *Client) FindPaymentByReference(ctx context.Context, reference string) (*Payment, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil, nil
	}
	path := "/payments?externalReference=" + url.QueryEscape(reference) + "&limit=1"

	var found list[Payment]
	if err := c.do(ctx, http.MethodGet, path, nil, &found); err != nil {
		return nil, err
	}
	for index := range found.Data {
		// A deleted charge is not a charge somebody can pay, so it must not be
		// mistaken for one this order already has.
		if !found.Data[index].Deleted {
			return &found.Data[index], nil
		}
	}
	return nil, nil
}

func (c *Client) CreatePayment(ctx context.Context, draft Payment) (*Payment, error) {
	var created Payment
	if err := c.do(ctx, http.MethodPost, "/payments", draft, &created); err != nil {
		return nil, err
	}
	if created.ID == "" {
		return nil, errors.New("asaas: payment created without an id")
	}
	return &created, nil
}

func (c *Client) GetPayment(ctx context.Context, paymentID string) (*Payment, error) {
	if err := validateID(paymentID); err != nil {
		return nil, err
	}
	var found Payment
	if err := c.do(ctx, http.MethodGet, "/payments/"+paymentID, nil, &found); err != nil {
		return nil, err
	}
	return &found, nil
}

// GetPixQRCode fetches the payload a bank app reads.
//
// A separate call because Asaas generates it separately; a charge exists and is
// payable through its invoice URL before this succeeds.
func (c *Client) GetPixQRCode(ctx context.Context, paymentID string) (*PixQRCode, error) {
	if err := validateID(paymentID); err != nil {
		return nil, err
	}
	var code PixQRCode
	if err := c.do(ctx, http.MethodGet, "/payments/"+paymentID+"/pixQrCode", nil, &code); err != nil {
		return nil, err
	}
	return &code, nil
}

// RefundPayment returns money. A zero value asks Asaas for a full refund.
func (c *Client) RefundPayment(ctx context.Context, paymentID string, valueReais float64, description string) error {
	if err := validateID(paymentID); err != nil {
		return err
	}
	body := map[string]any{"description": description}
	if valueReais > 0 {
		body["value"] = valueReais
	}
	return c.do(ctx, http.MethodPost, "/payments/"+paymentID+"/refund", body, nil)
}

// DeletePayment voids a charge that has not been paid.
func (c *Client) DeletePayment(ctx context.Context, paymentID string) error {
	if err := validateID(paymentID); err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, "/payments/"+paymentID, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if !c.Configured() {
		return ErrMissingAPIKey
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	// Asaas authenticates with its own header, not an Authorization bearer.
	request.Header.Set("access_token", c.apiKey)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		// No response at all. Transient by definition, and payment.Retryable
		// treats an unrecognised error as retryable, which is the right default:
		// giving up on a charge that would have succeeded costs a buyer their
		// tickets, one more attempt costs a round trip.
		return fmt.Errorf("asaas: %s %s: %w", method, path, err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("asaas: reading %s %s: %w", method, path, err)
	}

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return newResponseError(response.StatusCode, raw, response.Header)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("asaas: decoding %s %s: %w", method, path, err)
	}
	return nil
}

func newResponseError(status int, raw []byte, headers http.Header) error {
	failure := &ResponseError{StatusCode: status, Body: string(raw), retryAfter: retryAfterFrom(headers)}
	var envelope struct {
		Errors []APIError `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil {
		failure.Errors = envelope.Errors
	}
	return failure
}

func validateID(id string) error {
	if !idPattern.MatchString(strings.TrimSpace(id)) {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}

func onlyDigits(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if char >= '0' && char <= '9' {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}
