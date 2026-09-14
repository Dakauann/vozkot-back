package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	domain "vozkot/domain/notification"
	"vozkot/infra/config"
)

// A real HTTP server stands in for Vozko, the same way the email sender's tests
// stand one in for Resend. What is under test is the wire format, the sign-in
// dance and the verdict each refusal produces — none of which a mocked client
// would exercise, because a mock cannot disagree with the real request.

func testPhoneConfig(baseURL string) config.PhoneConfig {
	return config.PhoneConfig{
		BaseURL:         baseURL,
		Email:           "integration@example.com",
		Password:        "s3cret",
		WorkspaceID:     "ws_1",
		BusinessPhoneID: "bp_1",
		TemplateID:      "tpl_1",
		Timeout:         2 * time.Second,
		MaxRPS:          50,
	}
}

func codeMessage() domain.Message {
	return domain.Message{
		To:             "5584994409624",
		Name:           "Maria",
		Body:           "481905",
		Category:       string(domain.TemplateSignInCode),
		IdempotencyKey: "vch_abc:whatsapp:481905",
	}
}

// vozkoStub is Vozko's two relevant routes: sign in, then send.
type vozkoStub struct {
	logins   atomic.Int32
	sends    atomic.Int32
	sendBody atomic.Value // map[string]any
	headers  atomic.Value // http.Header

	// send decides what the send route answers, given the attempt number.
	send func(attempt int32, w http.ResponseWriter)
}

func (s *vozkoStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		s.logins.Add(1)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["email"] == "" || body["password"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Cookie mode is only entered when the caller asks for it; a client that
		// accidentally did would get no token in the body and never notice until a
		// send 401'd.
		if r.Header.Get("X-Auth-Mode") != "" {
			t.Errorf("sign-in asked for cookie mode: %q", r.Header.Get("X-Auth-Mode"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessToken": "token-" + strings.Repeat("x", int(s.logins.Load())),
			"tokenType":   "Bearer",
		})
	})
	mux.HandleFunc("/whatsapp/outreach/conversations", func(w http.ResponseWriter, r *http.Request) {
		attempt := s.sends.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.sendBody.Store(body)
		s.headers.Store(r.Header.Clone())
		s.send(attempt, w)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestVozkoCodeSenderSendsTheCodeAsTheTemplateParameter(t *testing.T) {
	stub := &vozkoStub{send: func(_ int32, w http.ResponseWriter) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entryId": "e1", "attemptId": "a1", "messageId": "wamid.X", "chargedMicros": 38000,
		})
	}}
	server := stub.server(t)

	sender := NewVozkoCodeSender(testPhoneConfig(server.URL))
	if sender == nil {
		t.Fatal("sender was not built from a complete configuration")
	}
	if err := sender.Send(context.Background(), codeMessage()); err != nil {
		t.Fatalf("send: %v", err)
	}

	body, _ := stub.sendBody.Load().(map[string]any)
	params, _ := body["parameters"].([]any)
	if len(params) != 1 || params[0] != "481905" {
		t.Fatalf("the code must be the single body parameter, got %v", body["parameters"])
	}
	if body["templateId"] != "tpl_1" || body["businessPhoneId"] != "bp_1" {
		t.Fatalf("configured ids were not sent: %v", body)
	}
	if body["phoneNumber"] != "5584994409624" {
		t.Fatalf("destination not sent as given: %v", body["phoneNumber"])
	}

	headers, _ := stub.headers.Load().(http.Header)
	// The workspace and the idempotency key are what make the send billable to
	// the right account and safe to retry. Both are easy to drop in a refactor and
	// neither failure is visible in development.
	if got := headers.Get("X-Workspace-ID"); got != "ws_1" {
		t.Errorf("X-Workspace-ID = %q, want ws_1", got)
	}
	if got := headers.Get("Idempotency-Key"); got != "vch_abc:whatsapp:481905" {
		t.Errorf("Idempotency-Key = %q", got)
	}
	if got := headers.Get("Authorization"); !strings.HasPrefix(got, "Bearer token-") {
		t.Errorf("Authorization = %q, want the token from sign-in", got)
	}
}

func TestVozkoCodeSenderSignsInOnceAndReusesTheToken(t *testing.T) {
	stub := &vozkoStub{send: func(_ int32, w http.ResponseWriter) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"attemptId":"a1"}`))
	}}
	server := stub.server(t)
	sender := NewVozkoCodeSender(testPhoneConfig(server.URL))

	for range 3 {
		if err := sender.Send(context.Background(), codeMessage()); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	// One sign-in for three sends. A client that logged in per send would work in
	// development and trip Vozko's login rate limiter on a busy sale day.
	if got := stub.logins.Load(); got != 1 {
		t.Fatalf("signed in %d times for 3 sends, want 1", got)
	}
}

func TestVozkoCodeSenderSignsInAgainWhenTheTokenExpires(t *testing.T) {
	stub := &vozkoStub{send: func(attempt int32, w http.ResponseWriter) {
		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":true,"message":"expired"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"attemptId":"a1"}`))
	}}
	server := stub.server(t)
	sender := NewVozkoCodeSender(testPhoneConfig(server.URL))

	if err := sender.Send(context.Background(), codeMessage()); err != nil {
		t.Fatalf("an expired token must be refreshed inside the call, got %v", err)
	}
	if got := stub.logins.Load(); got != 2 {
		t.Fatalf("signed in %d times, want 2 (initial + refresh)", got)
	}
	if got := stub.sends.Load(); got != 2 {
		t.Fatalf("sent %d times, want 2 (rejected + retried)", got)
	}
}

// The refusals matter more than the happy path: each one decides whether a job
// is parked or retried, and a code retried forever is a worker burning money
// while a customer waits.
func TestVozkoCodeSenderClassifiesRefusals(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code      string
		permanent bool
	}{
		// Cold-outbound guards. Both are correct for a sales message, wrong for a
		// login code, and neither improves on a retry.
		{"open window", http.StatusConflict, "window_already_open", true},
		{"spam cooldown", http.StatusConflict, "within_spam_window", true},
		{"no balance", http.StatusPaymentRequired, "insufficient_balance", true},
		{"template unusable", http.StatusUnprocessableEntity, "template_not_sendable", true},
		{"number not connected", http.StatusUnprocessableEntity, "phone_not_connected", true},
		{"no access", http.StatusForbidden, "forbidden", true},
		{"bad number", http.StatusBadRequest, "invalid_phone", true},

		// Worth another attempt: the first may yet land, or the ceiling lapses.
		{"already sending", http.StatusConflict, "send_in_progress", false},
		{"rate limited", http.StatusTooManyRequests, "rate_limited", false},
		{"platform down", http.StatusBadGateway, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &vozkoStub{send: func(_ int32, w http.ResponseWriter) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": true, "code": tc.code, "message": "refused",
				})
			}}
			server := stub.server(t)
			cfg := testPhoneConfig(server.URL)
			sender := NewVozkoCodeSender(cfg)

			err := sender.Send(context.Background(), codeMessage())
			if err == nil {
				t.Fatal("a refusal must be an error")
			}
			if permanent := errors.Is(err, domain.ErrUndeliverable); permanent != tc.permanent {
				t.Fatalf("permanent = %v, want %v (err: %v)", permanent, tc.permanent, err)
			}
			// A permanent refusal must not be attempted three times: the retries
			// exist for blips, and spending them on a settled answer delays the
			// queue's own, longer retry for nothing.
			if tc.permanent && stub.sends.Load() != 1 {
				t.Fatalf("attempted %d times for a permanent refusal, want 1", stub.sends.Load())
			}
		})
	}
}

// The code is a live credential. A platform that quotes the rejected payload back
// must not have it copied into an error that reaches logs and alerting.
func TestVozkoCodeSenderNeverLeaksTheCodeIntoAnError(t *testing.T) {
	stub := &vozkoStub{send: func(_ int32, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": true, "code": "template_not_sendable",
			"message": `parameter "481905" was rejected for template tpl_1`,
		})
	}}
	server := stub.server(t)
	sender := NewVozkoCodeSender(testPhoneConfig(server.URL))

	err := sender.Send(context.Background(), codeMessage())
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), "481905") {
		t.Fatalf("the code leaked into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("expected the code to be redacted, got %v", err)
	}
}

func TestVozkoCodeSenderRefusesAnIncompleteConfiguration(t *testing.T) {
	// Every field is load-bearing: without the ids there is nothing to send with,
	// without the credentials nothing to send as, without a workspace nobody to
	// bill. A partial configuration must yield no Sender at all, so the phone
	// routes answer 503 rather than failing per request.
	for _, missing := range []string{"BaseURL", "Email", "Password", "WorkspaceID", "BusinessPhoneID", "TemplateID"} {
		t.Run("without "+missing, func(t *testing.T) {
			cfg := testPhoneConfig("http://localhost:4000")
			switch missing {
			case "BaseURL":
				cfg.BaseURL = ""
			case "Email":
				cfg.Email = ""
			case "Password":
				cfg.Password = ""
			case "WorkspaceID":
				cfg.WorkspaceID = ""
			case "BusinessPhoneID":
				cfg.BusinessPhoneID = ""
			case "TemplateID":
				cfg.TemplateID = ""
			}
			if NewVozkoCodeSender(cfg) != nil {
				t.Fatalf("a configuration missing %s must not produce a sender", missing)
			}
		})
	}
}

// The renderer is what puts the code in Message.Body, and the sender is useless
// without it, so the two are checked against each other rather than separately.
func TestPhoneCodeRendererProducesTheCodeAndNothingElse(t *testing.T) {
	renderer := PhoneCodeRenderer{}

	body, err := renderer.Render(domain.ChannelWhatsApp, domain.TemplateSignInCode, map[string]any{"Code": "481905"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if body != "481905" {
		t.Fatalf("body = %q, want the bare code", body)
	}

	// An email template on a phone channel, and a code notification with no code,
	// are both permanent failures: no retry adds a template or invents a code.
	if _, err := renderer.Render(domain.ChannelWhatsApp, domain.TemplateOrderConfirmed, nil); !errors.Is(err, domain.ErrUnknownTemplate) {
		t.Errorf("a receipt must not render for a phone channel, got %v", err)
	}
	if _, err := renderer.Render(domain.ChannelWhatsApp, domain.TemplateSignInCode, nil); !errors.Is(err, domain.ErrUnknownTemplate) {
		t.Errorf("a missing code must refuse, got %v", err)
	}
	if _, err := renderer.Render(domain.ChannelEmail, domain.TemplateSignInCode, map[string]any{"Code": "1"}); !errors.Is(err, domain.ErrUnknownChannel) {
		t.Errorf("email is not this renderer's, got %v", err)
	}
}

// Channels is what lets one Renderer port serve two implementations. A channel
// with no renderer must refuse rather than fall through to email.
func TestChannelsDispatchesByChannel(t *testing.T) {
	renderers := NewChannels(map[domain.Channel]domain.Renderer{
		domain.ChannelWhatsApp: PhoneCodeRenderer{},
	})

	if _, err := renderers.Render(domain.ChannelWhatsApp, domain.TemplateSignInCode, map[string]any{"Code": "9"}); err != nil {
		t.Fatalf("whatsapp render: %v", err)
	}
	if _, err := renderers.Render(domain.ChannelEmail, domain.TemplateSignInCode, map[string]any{"Code": "9"}); !errors.Is(err, domain.ErrUnknownChannel) {
		t.Fatalf("an unregistered channel must refuse, got %v", err)
	}
}
