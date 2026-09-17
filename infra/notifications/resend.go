package notifications

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/resend/resend-go/v3"
	"golang.org/x/time/rate"

	domain "vozkot/domain/notification"
	"vozkot/infra/config"
)

const (
	// Per-attempt and overall bounds on one Send call.
	//
	// The overall budget is deliberately small next to the queue's. A worker
	// gives a job two minutes; spending all of it inside one provider call
	// would hold a slot that could be delivering somebody else's receipt, and
	// would delay the moment the durable retry, the one that survives this
	// process; takes over. Thirty seconds absorbs a blip; anything longer
	// than a blip is the job table's problem.
	emailSendPerAttemptTimeout = 12 * time.Second
	emailSendOverallTimeout    = 30 * time.Second
	emailSendMaxAttempts       = 4
	emailSendBaseBackoff       = 500 * time.Millisecond
	emailSendMaxBackoff        = 8 * time.Second

	// defaultResendMaxRPS stays under Resend's documented five requests a
	// second per team.
	defaultResendMaxRPS = 4
)

// EmailSender delivers notifications as email through Resend.
//
// Sends are rate limited client-side with a token bucket so a burst of
// confirmations queues instead of collecting 429s, and each send retries the
// transient failures, rate limit, request timeout, transport error, honouring
// the API's Retry-After hint. Anything that outlives that budget is handed back
// to the caller, because the job row retries better than a goroutine does: it
// survives a deploy.
//
// The bucket is PER PROCESS. Running N replicas multiplies the aggregate rate
// by N, so RESEND_MAX_REQUESTS_PER_SECOND is set to the account's limit divided
// by the replica count. Overshoot is not fatal; a 429 comes back with the
// provider's own Retry-After and is retried, but it is wasted work at exactly
// the moment there is the most of it.
type EmailSender struct {
	client      *resend.Client
	fromEmail   string
	fromName    string
	replyTo     string
	limiter     *rate.Limiter
	maxAttempts int
}

var _ domain.Sender = (*EmailSender)(nil)

// Option adjusts the sender at construction.
type Option func(*EmailSender)

// WithBaseURL points the client at another host.
//
// It exists for tests, which stand a real HTTP server in for Resend so that the
// retry, rate limit and wire format under test are the real ones. Production
// never sets it.
func WithBaseURL(raw string) Option {
	return func(e *EmailSender) {
		// The SDK resolves "emails" against this, so the trailing slash is
		// what makes the path append instead of replace.
		if !strings.HasSuffix(raw, "/") {
			raw += "/"
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			return
		}
		e.client.BaseURL = parsed
	}
}

// NewEmailSender builds the Resend-backed sender, or returns nil when no API
// key is configured. A nil sender is never registered, so nothing above can
// queue a message that could not have been delivered.
func NewEmailSender(cfg config.NotificationsConfig, opts ...Option) *EmailSender {
	if !cfg.Enabled() {
		return nil
	}
	maxRPS := cfg.MaxRPS
	if maxRPS <= 0 {
		maxRPS = defaultResendMaxRPS
	}
	sender := &EmailSender{
		client:      resend.NewClient(strings.TrimSpace(cfg.ResendAPIKey)),
		fromEmail:   strings.TrimSpace(cfg.FromEmail),
		fromName:    strings.TrimSpace(cfg.FromName),
		replyTo:     strings.TrimSpace(cfg.ReplyTo),
		limiter:     rate.NewLimiter(rate.Limit(maxRPS), maxRPS),
		maxAttempts: emailSendMaxAttempts,
	}
	for _, opt := range opts {
		opt(sender)
	}
	return sender
}

func (e *EmailSender) Channel() domain.Channel { return domain.ChannelEmail }

// Send delivers one message, retrying inside its own budget the failures that
// are worth retrying there.
func (e *EmailSender) Send(ctx context.Context, message domain.Message) error {
	if e == nil || e.client == nil {
		return fmt.Errorf("%w: RESEND_API_KEY is not set", domain.ErrNotConfigured)
	}
	recipients := parseRecipients(message.To)
	if len(recipients) == 0 {
		// No address is going to appear on a later attempt.
		return fmt.Errorf("%w: %w", domain.ErrUndeliverable, domain.ErrNoRecipient)
	}

	request := &resend.SendEmailRequest{
		From:    e.from(),
		To:      recipients,
		Subject: message.Subject,
		Html:    message.Body,
		ReplyTo: e.replyTo,
	}
	// The inline parts, which is how a QR reaches an inbox that renders it.
	// Resend turns an attachment with a ContentId into a multipart/related
	// part, and the body's cid: reference then resolves in Gmail, Outlook and
	// Apple Mail alike.
	for _, inline := range message.Inline {
		if inline.ContentID == "" || len(inline.Content) == 0 {
			continue
		}
		request.Attachments = append(request.Attachments, &resend.Attachment{
			Content:     inline.Content,
			Filename:    inline.Filename,
			ContentType: inline.ContentType,
			ContentId:   inline.ContentID,
		})
	}
	if tag := resendTag(message.Category); tag != "" {
		// Lets an operator read deliverability per kind of message in the
		// Resend dashboard rather than as one undifferentiated stream.
		request.Tags = []resend.Tag{{Name: "template", Value: tag}}
	}
	// Resend collapses a repeat of the same key, which closes the one window
	// the queue cannot: a worker killed after the provider accepted a send but
	// before the job row recorded it. The stale sweep re-runs that job, and the
	// key is what makes the second send arrive zero times instead of twice.
	options := &resend.SendEmailOptions{IdempotencyKey: message.IdempotencyKey}

	sendCtx, cancel := context.WithTimeout(ctx, emailSendOverallTimeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= e.maxAttempts; attempt++ {
		// Stay under the provider's rate limit; blocks until a token frees up
		// or the budget expires.
		if err := e.limiter.Wait(sendCtx); err != nil {
			if lastErr != nil {
				return fmt.Errorf("send email via Resend after %d attempt(s): %w", attempt-1, lastErr)
			}
			return fmt.Errorf("send email via Resend: %w", err)
		}

		attemptCtx, attemptCancel := context.WithTimeout(sendCtx, emailSendPerAttemptTimeout)
		_, err := e.client.Emails.SendWithOptions(attemptCtx, request, options)
		attemptCancel()
		if err == nil {
			return nil
		}
		lastErr = err

		if !isRetryableSendError(err) {
			return fmt.Errorf("send email via Resend: %w", err)
		}
		if attempt == e.maxAttempts {
			break
		}

		timer := time.NewTimer(sendBackoff(attempt, err))
		select {
		case <-sendCtx.Done():
			timer.Stop()
			return fmt.Errorf("send email via Resend after %d attempt(s): %w", attempt, lastErr)
		case <-timer.C:
		}
	}
	return fmt.Errorf("send email via Resend after %d attempt(s): %w", e.maxAttempts, lastErr)
}

// isRetryableSendError reports whether a failed send is worth trying again
// inside this call.
//
// A 429 arrives as a typed *resend.RateLimitError (matching resend.ErrRateLimit);
// request timeouts and transport errors are transient by definition. Everything
// else is left to the queue, including the SDK's untyped rendering of a 4xx
// and of a 5xx, which it does not tell apart. That is the right home for it:
// the queue waits minutes rather than seconds, and it is still waiting after a
// deploy.
func isRetryableSendError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, resend.ErrRateLimit) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// sendBackoff returns how long to wait before the next attempt: the API's own
// Retry-After when it gave one, capped, and exponential growth otherwise.
func sendBackoff(attempt int, err error) time.Duration {
	var rateLimited *resend.RateLimitError
	if errors.As(err, &rateLimited) {
		if seconds, parseErr := strconv.Atoi(strings.TrimSpace(rateLimited.RetryAfter)); parseErr == nil && seconds > 0 {
			if delay := time.Duration(seconds) * time.Second; delay < emailSendMaxBackoff {
				return delay
			}
			return emailSendMaxBackoff
		}
	}
	if attempt < 1 {
		attempt = 1
	}
	backoff := emailSendBaseBackoff << (attempt - 1)
	if backoff > emailSendMaxBackoff {
		return emailSendMaxBackoff
	}
	return backoff
}

// from renders the From header, e.g. "Vozko Tickets <no-reply@example.com>".
func (e *EmailSender) from() string {
	if e.fromName != "" {
		return fmt.Sprintf("%s <%s>", e.fromName, e.fromEmail)
	}
	return e.fromEmail
}

// parseRecipients accepts a single address or a comma-separated list and
// returns a clean slice.
func parseRecipients(raw string) []string {
	parts := strings.Split(raw, ",")
	addresses := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			addresses = append(addresses, trimmed)
		}
	}
	return addresses
}

// resendTag keeps a tag to what the API accepts: ASCII letters, digits,
// underscores and dashes. A rejected tag would fail the whole send, which is
// not a trade worth making for a dashboard filter.
func resendTag(category string) string {
	var cleaned strings.Builder
	for _, char := range category {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '_',
			char == '-':
			cleaned.WriteRune(char)
		}
	}
	return cleaned.String()
}
