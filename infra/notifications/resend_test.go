package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resend/resend-go/v3"

	domain "vozkot/domain/notification"
	"vozkot/infra/config"
)

// Resend is stubbed at the HTTP boundary and nowhere else, the same way the
// payment tests stub Mercado Pago: the requests go through the real SDK, so
// what is under test is this package's retry and rate-limit behaviour AND the
// wire format the provider will actually receive.

func newTestSender(t *testing.T, maxRPS int, handler http.HandlerFunc) *EmailSender {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	sender := NewEmailSender(config.NotificationsConfig{
		ResendAPIKey: "re_test_key",
		FromEmail:    "no-reply@tickets.example",
		FromName:     "Vozko Tickets",
		ReplyTo:      "suporte@tickets.example",
		MaxRPS:       maxRPS,
	})
	if sender == nil {
		t.Fatal("expected a sender for a configured API key")
	}
	// The SDK resolves "emails" against BaseURL, which must therefore keep its
	// trailing slash or the path is replaced instead of appended.
	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	sender.client.BaseURL = base
	return sender
}

func accepted(response http.ResponseWriter) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte(`{"id":"1b2c3d4e"}`))
}

func testMessage() domain.Message {
	return domain.Message{
		To:             "maria@exemplo.com.br",
		Name:           "Maria Souza",
		Subject:        "Pagamento confirmado — pedido A1B2C3D4",
		Body:           "<html><body>ok</body></html>",
		Category:       "order_confirmed",
		IdempotencyKey: "notification.send:order_confirmed:ord_1",
	}
}

func TestSendPostsWhatTheProviderExpects(t *testing.T) {
	var captured struct {
		path           string
		authorization  string
		idempotencyKey string
		body           map[string]any
	}
	sender := newTestSender(t, 0, func(response http.ResponseWriter, request *http.Request) {
		captured.path = request.URL.Path
		captured.authorization = request.Header.Get("Authorization")
		captured.idempotencyKey = request.Header.Get("Idempotency-Key")
		raw, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(raw, &captured.body)
		accepted(response)
	})

	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("send: %v", err)
	}

	if captured.path != "/emails" {
		t.Errorf("path = %q, want /emails", captured.path)
	}
	if captured.authorization != "Bearer re_test_key" {
		t.Errorf("authorization = %q", captured.authorization)
	}
	// The key is what makes a job the queue re-runs after a crash arrive zero
	// extra times rather than one.
	if captured.idempotencyKey != "notification.send:order_confirmed:ord_1" {
		t.Errorf("idempotency key = %q", captured.idempotencyKey)
	}
	if got := captured.body["from"]; got != "Vozko Tickets <no-reply@tickets.example>" {
		t.Errorf("from = %v", got)
	}
	if got := captured.body["reply_to"]; got != "suporte@tickets.example" {
		t.Errorf("reply_to = %v", got)
	}
	if got := captured.body["subject"]; got != "Pagamento confirmado — pedido A1B2C3D4" {
		t.Errorf("subject = %v", got)
	}
	if got := captured.body["html"]; got != "<html><body>ok</body></html>" {
		t.Errorf("html = %v", got)
	}
	to, _ := captured.body["to"].([]any)
	if len(to) != 1 || to[0] != "maria@exemplo.com.br" {
		t.Errorf("to = %v", captured.body["to"])
	}
	tags, _ := captured.body["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("tags = %v, want one", captured.body["tags"])
	}
	tag, _ := tags[0].(map[string]any)
	if tag["name"] != "template" || tag["value"] != "order_confirmed" {
		t.Errorf("tag = %v", tag)
	}
}

// A 429 is the provider asking for a moment, not a failure. It is retried
// inside the call, because handing a rate limit back to the queue would spend a
// whole minute of backoff on a wait measured in milliseconds.
func TestSendRetriesRateLimits(t *testing.T) {
	var attempts atomic.Int32
	sender := newTestSender(t, 0, func(response http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusTooManyRequests)
			_, _ = response.Write([]byte(`{"message":"Too many requests"}`))
			return
		}
		accepted(response)
	})

	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

// A rejected address is not retried here. Burning the in-call budget on it
// delays nothing but the same answer, and the queue's own attempts are what
// eventually park it where an operator can see it.
func TestSendDoesNotRetryRejectedRequests(t *testing.T) {
	var attempts atomic.Int32
	sender := newTestSender(t, 0, func(response http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = response.Write([]byte(`{"statusCode":422,"name":"validation_error","message":"Invalid to field"}`))
	})

	if err := sender.Send(context.Background(), testMessage()); err == nil {
		t.Fatal("expected an error for a rejected request")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

// A provider outage comes back as an ordinary error so the QUEUE retries it —
// with minutes of backoff, and across a deploy, which this call cannot do.
func TestSendHandsProviderOutagesBackToTheQueue(t *testing.T) {
	sender := newTestSender(t, 0, func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = response.Write([]byte(`{"message":"service unavailable"}`))
	})

	err := sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("expected an error for a provider outage")
	}
	// Not permanent: the queue must keep trying this one.
	if errors.Is(err, domain.ErrUndeliverable) {
		t.Error("a provider outage must not be reported as undeliverable")
	}
}

// The token bucket is what keeps a burst of confirmations from becoming a wall
// of 429s. Four per second, eight messages: the first four go at once and the
// rest are spaced, so the batch cannot finish in under a second.
func TestSendIsRateLimited(t *testing.T) {
	sender := newTestSender(t, 4, func(response http.ResponseWriter, _ *http.Request) {
		accepted(response)
	})

	started := time.Now()
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := sender.Send(context.Background(), testMessage()); err != nil {
				t.Errorf("send: %v", err)
			}
		}()
	}
	group.Wait()

	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("eight sends at 4/s took %v; the rate limit is not being applied", elapsed)
	}
}

// The caller's context wins. A worker shutting down must not be held by a
// provider call that has stopped answering.
func TestSendRespectsTheCallerContext(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sender := newTestSender(t, 0, func(response http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
		accepted(response)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := sender.Send(ctx, testMessage()); err == nil {
		t.Fatal("expected an error once the caller's context expired")
	}
	if elapsed := time.Since(started); elapsed > emailSendOverallTimeout {
		t.Fatalf("send outlived the caller's deadline by %v", elapsed)
	}
}

func TestSendWithoutAnAPIKeyIsNotBuilt(t *testing.T) {
	if sender := NewEmailSender(config.NotificationsConfig{FromEmail: "a@b.com"}); sender != nil {
		t.Fatal("expected no sender when RESEND_API_KEY is unset")
	}
}

// An empty recipient can only ever fail, so it is reported as undeliverable and
// the job is parked rather than retried twenty times.
func TestSendRejectsAnEmptyRecipient(t *testing.T) {
	sender := newTestSender(t, 0, func(response http.ResponseWriter, _ *http.Request) {
		t.Error("the provider must not be called without a recipient")
		accepted(response)
	})

	message := testMessage()
	message.To = "   "
	err := sender.Send(context.Background(), message)
	if !errors.Is(err, domain.ErrUndeliverable) {
		t.Fatalf("err = %v, want ErrUndeliverable", err)
	}
}

func TestFromHeader(t *testing.T) {
	cases := []struct {
		name      string
		fromEmail string
		fromName  string
		want      string
	}{
		{"name and email", "no-reply@tickets.example", "Vozko Tickets", "Vozko Tickets <no-reply@tickets.example>"},
		{"email only", "no-reply@tickets.example", "", "no-reply@tickets.example"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sender := &EmailSender{fromEmail: testCase.fromEmail, fromName: testCase.fromName}
			if got := sender.from(); got != testCase.want {
				t.Fatalf("from() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestParseRecipients(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"single", "a@x.com", []string{"a@x.com"}},
		{"comma separated with spaces", "a@x.com, b@y.com ,c@z.com", []string{"a@x.com", "b@y.com", "c@z.com"}},
		{"empty entries dropped", "a@x.com,,  ,b@y.com", []string{"a@x.com", "b@y.com"}},
		{"blank", "   ", []string{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parseRecipients(testCase.in); !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("parseRecipients(%q) = %#v, want %#v", testCase.in, got, testCase.want)
			}
		})
	}
}

func TestIsRetryableSendError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limit", &resend.RateLimitError{RetryAfter: "1"}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"validation", errors.New("[ERROR]: invalid `to` field"), false},
		{"unknown api error", errors.New("[ERROR]: something"), false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isRetryableSendError(testCase.err); got != testCase.want {
				t.Fatalf("isRetryableSendError(%v) = %v, want %v", testCase.err, got, testCase.want)
			}
		})
	}
}

func TestSendBackoff(t *testing.T) {
	// Honour Retry-After when it is present and within the cap.
	if got := sendBackoff(1, &resend.RateLimitError{RetryAfter: "2"}); got != 2*time.Second {
		t.Fatalf("retry-after backoff = %v, want 2s", got)
	}
	// Cap an excessive Retry-After: the queue is the right place for a long wait.
	if got := sendBackoff(1, &resend.RateLimitError{RetryAfter: "100"}); got != emailSendMaxBackoff {
		t.Fatalf("capped retry-after = %v, want %v", got, emailSendMaxBackoff)
	}
	// Exponential growth otherwise.
	if got := sendBackoff(1, errors.New("boom")); got != emailSendBaseBackoff {
		t.Fatalf("attempt 1 backoff = %v, want %v", got, emailSendBaseBackoff)
	}
	if got := sendBackoff(3, errors.New("boom")); got != emailSendBaseBackoff<<2 {
		t.Fatalf("attempt 3 backoff = %v, want %v", got, emailSendBaseBackoff<<2)
	}
	if got := sendBackoff(30, errors.New("boom")); got != emailSendMaxBackoff {
		t.Fatalf("attempt 30 backoff = %v, want the ceiling %v", got, emailSendMaxBackoff)
	}
}

func TestResendTagKeepsOnlyAcceptedCharacters(t *testing.T) {
	if got := resendTag("order_confirmed"); got != "order_confirmed" {
		t.Fatalf("tag = %q", got)
	}
	if got := resendTag("pedido pago!·2026"); got != "pedidopago2026" {
		t.Fatalf("tag = %q", got)
	}
}
