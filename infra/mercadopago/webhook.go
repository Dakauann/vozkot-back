package mercadopago

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Notification is the envelope Mercado Pago POSTs to the webhook URL.
//
// It carries no payment state: only a resource id under data.id. Acting on it
// therefore always requires a GET /v1/payments/{id}, and that is a feature —
// state that is read back cannot be forged by whoever sent the request.
//
// Reference: https://www.mercadopago.com/developers/en/docs/your-integrations/notifications/webhooks
type Notification struct {
	ID          json.Number `json:"id"`
	LiveMode    bool        `json:"live_mode"`
	Type        string      `json:"type"`
	Topic       string      `json:"topic"`
	DateCreated string      `json:"date_created"`
	UserID      json.Number `json:"user_id"`
	APIVersion  string      `json:"api_version"`
	Action      string      `json:"action"`
	Data        struct {
		ID string `json:"id"`
	} `json:"data"`
	// Resource is only populated by the older IPN format, where the id arrives
	// as a URL rather than an id.
	Resource string `json:"resource"`
}

const NotificationTypePayment = "payment"

// ResourceID returns the payment id a notification refers to, normalising the
// two envelope formats Mercado Pago still emits.
func (n *Notification) ResourceID() string {
	if n == nil {
		return ""
	}
	if id := strings.TrimSpace(n.Data.ID); id != "" {
		return id
	}
	if resource := strings.TrimSpace(n.Resource); resource != "" {
		if index := strings.LastIndex(resource, "/"); index >= 0 && index+1 < len(resource) {
			return resource[index+1:]
		}
		return resource
	}
	return ""
}

// NormalizedType tolerates the legacy "topic" field older configurations send.
func (n *Notification) NormalizedType() string {
	if n == nil {
		return ""
	}
	if value := strings.ToLower(strings.TrimSpace(n.Type)); value != "" {
		return value
	}
	return strings.ToLower(strings.TrimSpace(n.Topic))
}

func (n *Notification) IsPayment() bool {
	return n.NormalizedType() == NotificationTypePayment
}

// DeliveryID identifies one delivery, for deduplication. Mercado Pago's own
// notification id when present, falling back to the resource it concerns.
func (n *Notification) DeliveryID() string {
	if n == nil {
		return ""
	}
	if id := strings.TrimSpace(n.ID.String()); id != "" && id != "0" {
		return id
	}
	return n.ResourceID()
}

var ErrNotificationMissingResourceID = errors.New("mercadopago: notification has no resource id")

// ParseNotification decodes a webhook body, falling back to the query string
// for the legacy IPN format, which sends "?topic=payment&id=123" and no body.
func ParseNotification(body []byte, query url.Values) (*Notification, error) {
	var notification Notification
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &notification); err != nil {
			return nil, fmt.Errorf("mercadopago: invalid notification body: %w", err)
		}
	}

	if notification.ResourceID() == "" && query != nil {
		if id := strings.TrimSpace(query.Get("data.id")); id != "" {
			notification.Data.ID = id
		} else if id := strings.TrimSpace(query.Get("id")); id != "" {
			notification.Data.ID = id
		}
	}
	if notification.NormalizedType() == "" && query != nil {
		if value := strings.TrimSpace(query.Get("type")); value != "" {
			notification.Type = value
		} else if value := strings.TrimSpace(query.Get("topic")); value != "" {
			notification.Topic = value
		}
	}

	if notification.ResourceID() == "" {
		return nil, ErrNotificationMissingResourceID
	}
	return &notification, nil
}

// SignatureReason describes why verification failed. It is logged rather than
// returned to the sender, who must never learn which part of a forgery was
// wrong.
type SignatureReason string

const (
	ReasonMissingSecret           SignatureReason = "MissingSecret"
	ReasonMissingSignatureHeader  SignatureReason = "MissingSignatureHeader"
	ReasonMalformedSignature      SignatureReason = "MalformedSignatureHeader"
	ReasonMissingTimestamp        SignatureReason = "MissingTimestamp"
	ReasonMissingHash             SignatureReason = "MissingHash"
	ReasonSignatureMismatch       SignatureReason = "SignatureMismatch"
	ReasonTimestampOutOfTolerance SignatureReason = "TimestampOutOfTolerance"
)

type SignatureError struct {
	Reason    SignatureReason
	RequestID string
	Timestamp string
}

func (e *SignatureError) Error() string {
	return "mercadopago: invalid webhook signature: " + string(e.Reason)
}

// ErrInvalidSignature is the sentinel every SignatureError satisfies.
var ErrInvalidSignature = errors.New("mercadopago: invalid webhook signature")

func (e *SignatureError) Is(target error) bool { return target == ErrInvalidSignature }

var signatureVersionKey = regexp.MustCompile(`^v\d+$`)

// VerifySignature validates the x-signature header Mercado Pago sends with
// every webhook.
//
// The signed manifest is "id:<data.id>;request-id:<x-request-id>;ts:<ts>;" with
// empty components omitted entirely, HMAC-SHA256 over the application's secret,
// hex encoded, compared in constant time.
//
// Two details are easy to get wrong:
//
//   - data.id comes from the QUERY STRING. The two usually agree, but Mercado
//     Pago signs the query parameter, so that is what is verified.
//   - The documentation says an alphanumeric id is lowercased before hashing,
//     while Mercado Pago's own SDK hashes it verbatim. Payment ids are numeric
//     so the two agree today; both are tried so a future non-numeric id cannot
//     silently start rejecting every webhook.
//
// tolerance <= 0 disables the replay window, and that is the default on
// purpose: Mercado Pago retries a failed delivery for hours WITHOUT re-signing,
// so a tight window rejects exactly the retries that matter most.
func VerifySignature(xSignature, xRequestID, dataID, secret string, tolerance time.Duration, now time.Time) error {
	_, err := VerifySignatureAny(xSignature, xRequestID, []string{dataID}, secret, tolerance, now)
	return err
}

// VerifySignatureAny verifies against several candidate ids and reports which
// one matched.
//
// Trying several candidates is not a weakening: each is a full HMAC comparison
// against the same secret, so an attacker who cannot produce a valid HMAC still
// cannot pass. What it buys is the matched id, so the caller acts on the id that
// was actually signed rather than verifying one value and processing another.
func VerifySignatureAny(xSignature, xRequestID string, dataIDs []string, secret string, tolerance time.Duration, now time.Time) (string, error) {
	xSignature = strings.TrimSpace(xSignature)
	xRequestID = strings.TrimSpace(xRequestID)
	secret = strings.TrimSpace(secret)

	if secret == "" {
		// Fail closed. An unconfigured secret must never turn the endpoint into
		// an open one anyone can use to mark orders paid.
		return "", &SignatureError{Reason: ReasonMissingSecret, RequestID: xRequestID}
	}
	if xSignature == "" {
		return "", &SignatureError{Reason: ReasonMissingSignatureHeader, RequestID: xRequestID}
	}

	timestamp, hashes := parseSignatureHeader(xSignature)
	if timestamp == "" && len(hashes) == 0 {
		return "", &SignatureError{Reason: ReasonMalformedSignature, RequestID: xRequestID}
	}
	if timestamp == "" {
		return "", &SignatureError{Reason: ReasonMissingTimestamp, RequestID: xRequestID}
	}
	timestampMillis, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return "", &SignatureError{Reason: ReasonMalformedSignature, RequestID: xRequestID, Timestamp: timestamp}
	}

	received := hashes["v1"]
	if received == "" {
		return "", &SignatureError{Reason: ReasonMissingHash, RequestID: xRequestID, Timestamp: timestamp}
	}

	type candidate struct {
		manifest string
		dataID   string
	}
	var candidates []candidate
	seen := map[string]struct{}{}
	add := func(id string) {
		manifest := buildManifest(id, xRequestID, timestamp)
		if _, duplicate := seen[manifest]; duplicate {
			return
		}
		seen[manifest] = struct{}{}
		candidates = append(candidates, candidate{manifest: manifest, dataID: id})
	}
	for _, raw := range dataIDs {
		id := strings.TrimSpace(raw)
		add(strings.ToLower(id))
		add(id)
	}
	if len(candidates) == 0 {
		add("")
	}

	matchedID := ""
	matched := false
	for _, item := range candidates {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(item.manifest))
		computed := hex.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(computed), []byte(received)) {
			matchedID = item.dataID
			matched = true
			break
		}
	}
	if !matched {
		return "", &SignatureError{Reason: ReasonSignatureMismatch, RequestID: xRequestID, Timestamp: timestamp}
	}

	if tolerance > 0 {
		if now.IsZero() {
			now = time.Now()
		}
		drift := now.UnixMilli() - timestampMillis
		if drift < 0 {
			drift = -drift
		}
		if drift > tolerance.Milliseconds() {
			return "", &SignatureError{Reason: ReasonTimestampOutOfTolerance, RequestID: xRequestID, Timestamp: timestamp}
		}
	}

	return matchedID, nil
}

// parseSignatureHeader splits "ts=...,v1=..." into its parts, ignoring unknown
// keys so a future added component cannot break verification.
func parseSignatureHeader(header string) (timestamp string, hashes map[string]string) {
	hashes = map[string]string{}
	for _, part := range strings.Split(header, ",") {
		rawKey, rawValue, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(rawKey))
		value := strings.TrimSpace(rawValue)
		if key == "" || value == "" {
			continue
		}
		if key == "ts" {
			timestamp = value
			continue
		}
		if signatureVersionKey.MatchString(key) {
			hashes[key] = value
		}
	}
	return timestamp, hashes
}

// buildManifest assembles the signed string, dropping empty components. The
// trailing semicolon is part of the format.
func buildManifest(dataID, requestID, timestamp string) string {
	parts := make([]string, 0, 3)
	if dataID != "" {
		parts = append(parts, "id:"+dataID)
	}
	if requestID != "" {
		parts = append(parts, "request-id:"+requestID)
	}
	parts = append(parts, "ts:"+timestamp)
	return strings.Join(parts, ";") + ";"
}

// SignForTesting produces the header Mercado Pago would send. It exists so the
// webhook tests exercise the real verifier instead of a stub of it.
func SignForTesting(dataID, requestID, secret string, at time.Time) string {
	timestamp := strconv.FormatInt(at.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(buildManifest(dataID, requestID, timestamp)))
	return "ts=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
