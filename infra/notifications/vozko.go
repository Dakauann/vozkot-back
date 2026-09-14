package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	domain "vozkot/domain/notification"
	"vozkot/infra/config"
)

// Delivering a verification code over WhatsApp through Vozko's platform.
//
// Vozko owns the WhatsApp Business account, the approved templates and the
// balance the sends are billed against, so this is a client for an endpoint that
// already exists there rather than a second WhatsApp integration. Specifically it
// is /whatsapp/outreach/conversations, the platform's one paid single-target
// template send: it is synchronous, it honours an idempotency key, and it answers
// with a specific code when it refuses. The campaign quick-send route was the
// other candidate and is unusable for codes — it skips a repeat to the same
// number as a duplicate, serialises behind a per-campaign lock, and reports
// nothing per message.
//
// TWO THINGS ABOUT IT ARE UNCOMFORTABLE and neither is this package's to fix:
//
//  1. It authenticates as a PERSON. Vozko has no API key or service account, so
//     this signs in with an operator's email and password and carries the access
//     token it gets back. Use an account created for this integration, not a
//     human's, and give it one workspace.
//  2. It is built for cold outbound, so it refuses a send when the recipient
//     already has an open 24h WhatsApp window, or when the workspace's own spam
//     cooldown covers them. Both are correct for a sales message and wrong for a
//     login code, and both surface here as a permanent failure with a loud log
//     line, because pretending a code was delivered would be worse.
//
// Everything else follows the manners of the email sender beside it: a
// client-side rate limit, a bounded budget per call, retries only for what is
// worth retrying inside one call, and everything else handed back to the queue,
// which retries better because it survives a deploy.
type VozkoCodeSender struct {
	cfg     config.PhoneConfig
	client  *http.Client
	limiter *rate.Limiter

	// mu guards the cached access token. Workers deliver concurrently, so without
	// it a token refresh races with every other in-flight send.
	mu    sync.Mutex
	token string
}

var _ domain.Sender = (*VozkoCodeSender)(nil)

const (
	// One call's budget. Deliberately short: somebody is watching a screen for
	// this code, and a job row waiting two minutes inside one HTTP call is a
	// worker slot not delivering anybody else's.
	codeSendPerAttemptTimeout = 10 * time.Second
	codeSendOverallTimeout    = 25 * time.Second
	codeSendMaxAttempts       = 3
	codeSendBaseBackoff       = 400 * time.Millisecond
	codeSendMaxBackoff        = 4 * time.Second

	defaultCodeMaxRPS = 10
)

// NewVozkoCodeSender builds the sender, or returns nil when the integration is
// not configured.
//
// A nil Sender is never registered, so nothing above can queue a message that
// could not have been delivered — and phone confirmation keeps answering 503,
// which is the honest reply when no channel exists.
func NewVozkoCodeSender(cfg config.PhoneConfig) *VozkoCodeSender {
	if !cfg.Enabled() {
		return nil
	}
	maxRPS := cfg.MaxRPS
	if maxRPS <= 0 {
		maxRPS = defaultCodeMaxRPS
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = codeSendPerAttemptTimeout
	}
	return &VozkoCodeSender{
		cfg: cfg,
		client: &http.Client{
			Timeout: timeout,
			// No redirect following. A redirect on an authenticated POST would
			// resend the credential, and the code, to whatever host the response
			// named.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		limiter: rate.NewLimiter(rate.Limit(maxRPS), maxRPS),
	}
}

// Channel is WhatsApp, and only WhatsApp.
//
// The endpoint behind this sender delivers an approved WhatsApp template; there is
// nothing SMS about it. When SMS arrives it is a second Sender registered for
// domain.ChannelSMS — the domain, the renderer and the use case already handle
// that channel — and not a flag here. An adapter that claimed both channels and
// sent one would make a screen say "check your SMS" while a WhatsApp message
// arrived.
func (s *VozkoCodeSender) Channel() domain.Channel { return domain.ChannelWhatsApp }

// Send delivers one code.
func (s *VozkoCodeSender) Send(ctx context.Context, message domain.Message) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("%w: VOZKO_BASE_URL is not set", domain.ErrNotConfigured)
	}
	destination := strings.TrimSpace(message.To)
	if destination == "" {
		// No number is going to appear on a later attempt.
		return fmt.Errorf("%w: %w", domain.ErrUndeliverable, domain.ErrNoRecipient)
	}
	// The body IS the code on this channel: the phone renderer produced it, and
	// the template at Meta owns every other word of the message.
	code := strings.TrimSpace(message.Body)
	if code == "" {
		return fmt.Errorf("%w: message carries no code", domain.ErrUndeliverable)
	}

	payload, err := json.Marshal(startConversationRequest{
		BusinessPhoneID: s.cfg.BusinessPhoneID,
		TemplateID:      s.cfg.TemplateID,
		PhoneNumber:     destination,
		Name:            strings.TrimSpace(message.Name),
		// The template's single body variable. The platform copies it onto the
		// authentication template's OTP button itself, which is why nothing here
		// knows what a button component is.
		Parameters: []string{code},
	})
	if err != nil {
		return fmt.Errorf("%w: encode code delivery: %w", domain.ErrUndeliverable, err)
	}

	sendCtx, cancel := context.WithTimeout(ctx, codeSendOverallTimeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= codeSendMaxAttempts; attempt++ {
		if err := s.limiter.Wait(sendCtx); err != nil {
			if lastErr != nil {
				return fmt.Errorf("deliver code after %d attempt(s): %w", attempt-1, lastErr)
			}
			return fmt.Errorf("deliver code: %w", err)
		}

		err := s.post(sendCtx, payload, message.IdempotencyKey, code)
		if err == nil {
			return nil
		}
		lastErr = err

		// A permanent refusal is handed straight back: the Service parks the job
		// rather than spending three attempts learning the same thing.
		if errors.Is(err, domain.ErrUndeliverable) {
			return err
		}
		if attempt == codeSendMaxAttempts {
			break
		}

		timer := time.NewTimer(codeBackoff(attempt, err))
		select {
		case <-sendCtx.Done():
			timer.Stop()
			return fmt.Errorf("deliver code after %d attempt(s): %w", attempt, lastErr)
		case <-timer.C:
		}
	}
	return fmt.Errorf("deliver code after %d attempt(s): %w", codeSendMaxAttempts, lastErr)
}

// post makes one authenticated attempt.
//
// A 401 is retried ONCE with a fresh sign-in, inside this attempt rather than by
// returning: an access token expiring mid-flight is the expected steady state of
// authenticating as a user, and it should cost a round trip rather than a queue
// retry with backoff.
func (s *VozkoCodeSender) post(ctx context.Context, payload []byte, idempotencyKey, code string) error {
	token, err := s.accessToken(ctx, false)
	if err != nil {
		return err
	}

	status, body, err := s.attempt(ctx, payload, idempotencyKey, token)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		if token, err = s.accessToken(ctx, true); err != nil {
			return err
		}
		if status, body, err = s.attempt(ctx, payload, idempotencyKey, token); err != nil {
			return err
		}
	}
	return classifyCodeDelivery(status, body, code)
}

// attempt performs the request and reads the response. The returned error is a
// transport failure only; an HTTP status is data, not an error.
func (s *VozkoCodeSender) attempt(ctx context.Context, payload []byte, idempotencyKey, token string) (int, []byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, codeSendPerAttemptTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(attemptCtx, http.MethodPost,
		s.cfg.BaseURL+"/whatsapp/outreach/conversations", bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: build code delivery request: %w", domain.ErrUndeliverable, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	// The account may belong to more than one workspace, and this header is how
	// the platform resolves which one owns and pays for the send.
	request.Header.Set("X-Workspace-ID", s.cfg.WorkspaceID)
	// The same key the queue deduplicated the job on, so the two layers agree on
	// what "the same message" means. It closes the window the queue cannot: a
	// worker killed after the platform accepted the send but before the job row
	// recorded it, whose job the stale sweep re-runs.
	request.Header.Set("Idempotency-Key", idempotencyKey)

	response, err := s.client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("deliver code: %w", err)
	}
	defer response.Body.Close()

	// Bounded: an error body is a couple of hundred bytes, and an unbounded read
	// of a misconfigured URL's response is a worker eating a web page.
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	return response.StatusCode, body, nil
}

// accessToken returns a usable token, signing in when there is none or when the
// caller says the cached one was rejected.
func (s *VozkoCodeSender) accessToken(ctx context.Context, force bool) (string, error) {
	s.mu.Lock()
	cached := s.token
	s.mu.Unlock()
	if cached != "" && !force {
		return cached, nil
	}

	token, err := s.login(ctx)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.token = token
	s.mu.Unlock()
	return token, nil
}

// login exchanges the configured credentials for an access token.
func (s *VozkoCodeSender) login(ctx context.Context) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"email":    s.cfg.Email,
		"password": s.cfg.Password,
	})
	if err != nil {
		return "", fmt.Errorf("%w: encode sign-in: %w", domain.ErrUndeliverable, err)
	}

	loginCtx, cancel := context.WithTimeout(ctx, codeSendPerAttemptTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(loginCtx, http.MethodPost,
		s.cfg.BaseURL+"/auth/login", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("%w: build sign-in request: %w", domain.ErrUndeliverable, err)
	}
	request.Header.Set("Content-Type", "application/json")
	// Deliberately NOT X-Auth-Mode: cookie. In cookie mode the platform sets the
	// token as a cookie and leaves it out of the body, and a server holding a
	// cookie jar for one account is a worse thing to maintain than a bearer token.

	response, err := s.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("sign in to deliver code: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))

	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		// Permanent: wrong credentials do not become right on the next attempt.
		// Logged loudly because the only symptom otherwise is phone confirmation
		// quietly failing for everybody.
		log.Printf("notifications: WARNING Vozko rejected the integration sign-in (%d); codes cannot be delivered until VOZKO_EMAIL and VOZKO_PASSWORD are correct", response.StatusCode)
		return "", fmt.Errorf("%w: sign-in rejected", domain.ErrUndeliverable)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		// Everything else — a 429 on the login route, a 5xx, a proxy error — is
		// worth another attempt.
		return "", fmt.Errorf("sign in to deliver code: unexpected status %d", response.StatusCode)
	}

	var decoded struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", fmt.Errorf("sign in to deliver code: unreadable response: %w", err)
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		return "", fmt.Errorf("%w: sign-in returned no access token", domain.ErrUndeliverable)
	}
	return decoded.AccessToken, nil
}

// startConversationRequest is the platform's paid single-target template send.
type startConversationRequest struct {
	BusinessPhoneID string   `json:"businessPhoneId"`
	TemplateID      string   `json:"templateId"`
	PhoneNumber     string   `json:"phoneNumber"`
	Name            string   `json:"name,omitempty"`
	Parameters      []string `json:"parameters,omitempty"`
}

// codedError is the platform's error envelope, which carries a machine-readable
// code beside the human message.
type codedError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// classifyCodeDelivery turns one response into the verdict the queue needs.
//
// The split is the whole point of this function, and it is not the usual
// "4xx permanent, 5xx retry": several of the platform's 4xx answers here mean
// "this code will never be delivered" and must park the job immediately, while a
// couple of its 409s mean "try again shortly". Each of the permanent ones is
// logged, because every one of them is a configuration or policy problem on the
// platform side that is otherwise invisible from here — the customer just never
// receives a code.
//
// Nothing from the response body is echoed into the returned error beyond the
// platform's own code and message, and the code is scrubbed from both: a provider
// can quote back the payload it rejected, and on this path the payload is a live
// credential.
func classifyCodeDelivery(status int, body []byte, code string) error {
	var decoded codedError
	_ = json.Unmarshal(body, &decoded)
	detail := redactCode(strings.TrimSpace(decoded.Message), code)
	if detail == "" {
		detail = fmt.Sprintf("status %d", status)
	}

	if status >= 200 && status <= 299 {
		return nil
	}

	switch decoded.Code {
	case "window_already_open":
		// The recipient already has an open 24h WhatsApp window, so the platform
		// refuses to charge for a template and points an operator at the free
		// composer instead. Correct for cold outbound, useless for a login code:
		// this person simply cannot receive one this way until the window lapses.
		log.Printf("notifications: WARNING code not delivered, the recipient has an open WhatsApp window and the platform's outreach endpoint refuses a paid template for them. They cannot receive a code on this channel right now.")
		return fmt.Errorf("%w: %s", domain.ErrUndeliverable, detail)

	case "within_spam_window":
		// The workspace's own campaign cooldown. A second code the next day is
		// exactly what a resend is, so this will happen in normal use.
		log.Printf("notifications: WARNING code not delivered, the workspace's campaign spam cooldown covers this recipient. Lower it for the number that sends codes, or verification resends will keep failing.")
		return fmt.Errorf("%w: %s", domain.ErrUndeliverable, detail)

	case "insufficient_balance":
		log.Printf("notifications: WARNING code not delivered, the Vozko workspace balance is exhausted. Top it up; every phone confirmation fails until then.")
		return fmt.Errorf("%w: %s", domain.ErrUndeliverable, detail)

	case "template_not_sendable", "pricing_unavailable", "template_phone_mismatch", "phone_not_connected":
		log.Printf("notifications: WARNING code not delivered, the platform cannot use the configured template or number (%s: %s). Check VOZKO_OTP_TEMPLATE_ID and VOZKO_BUSINESS_PHONE_ID.", decoded.Code, detail)
		return fmt.Errorf("%w: %s", domain.ErrUndeliverable, detail)

	case "forbidden", "lead_blocked", "not_found":
		log.Printf("notifications: WARNING code not delivered (%s: %s). Check that the integration account has template-send permission on workspace %s and that the configured ids exist.", decoded.Code, detail, "VOZKO_WORKSPACE_ID")
		return fmt.Errorf("%w: %s", domain.ErrUndeliverable, detail)

	case "invalid_phone", "idempotency_key_required":
		return fmt.Errorf("%w: %s", domain.ErrUndeliverable, detail)

	case "send_in_progress":
		// The same idempotency key is still in flight. Worth another attempt, and
		// never worth a second message: the first may yet land.
		return fmt.Errorf("deliver code: %s", detail)

	case "rate_limited":
		return fmt.Errorf("deliver code: %s", detail)
	}

	// No recognised code. Fall back to the status, and lean towards retrying:
	// parking a login code because a proxy returned an unexpected 4xx is the more
	// expensive mistake, and the queue gives up eventually on its own.
	if status == http.StatusBadRequest || status == http.StatusNotFound {
		return fmt.Errorf("%w: %s", domain.ErrUndeliverable, detail)
	}
	return fmt.Errorf("deliver code: %s", detail)
}

// codeBackoff returns how long to wait before the next attempt: the platform's
// own Retry-After when it gave one, capped, and exponential growth otherwise.
//
// Retry-After is not parsed from the response here — the platform sends it on its
// rate-limit replies and this client has already discarded the headers by the time
// a verdict is formed — so the growth curve carries it. Kept deliberately short:
// three attempts inside 25 seconds, then the job row takes over with minutes.
func codeBackoff(attempt int, _ error) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := codeSendBaseBackoff << (attempt - 1)
	if backoff > codeSendMaxBackoff {
		return codeSendMaxBackoff
	}
	return backoff
}

// redactCode removes the code from a string.
//
// Blunt on purpose: a code is a short token and a platform echoing it back can
// spell it inside a longer sentence, so substring replacement is the only form
// that reliably catches it. Very short values are left alone, because replacing a
// one or two character string would corrupt the message and tell nobody anything.
func redactCode(text, code string) string {
	code = strings.TrimSpace(code)
	if len(code) < 3 || text == "" {
		return text
	}
	return strings.ReplaceAll(text, code, "[redacted]")
}
