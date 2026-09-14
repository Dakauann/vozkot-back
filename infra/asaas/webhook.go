package asaas

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
)

// Asaas webhooks.
//
// Authentication is a SHARED TOKEN the operator sets on both sides and Asaas
// echoes in the `asaas-access-token` header. It is not a signature: it does not
// cover the body, so it proves the sender knows the secret and nothing about
// what they sent.
//
// That is weaker than Mercado Pago's HMAC, and it is the reason this integration
// keeps the rule it already had; a webhook is a doorbell, never a delivery.
// The body is used for exactly one thing, the id to read back. Every fact that
// moves money or stock is re-read from the API with our own credentials, so a
// forged body is worth nothing even to somebody who obtained the token.
const TokenHeader = "asaas-access-token"

var (
	ErrMissingWebhookToken  = errors.New("asaas: webhook token is not configured")
	ErrInvalidWebhookToken  = errors.New("asaas: webhook token does not match")
	ErrUnusableNotification = errors.New("asaas: notification carries no payment id")
)

// Notification is the part of an Asaas webhook body this system reads.
//
// Asaas ships the whole payment, and this deliberately ignores nearly all of
// it. Decoding the status here and acting on it would mean trusting an
// unsigned body about whether money arrived.
type Notification struct {
	ID      string `json:"id"`
	Event   string `json:"event"`
	Payment struct {
		ID                string `json:"id"`
		ExternalReference string `json:"externalReference"`
		Status            string `json:"status"`
	} `json:"payment"`
}

// VerifyToken checks the shared secret in constant time.
//
// Constant-time because a byte-by-byte compare on a secret is a timing oracle:
// an attacker who can measure the difference recovers the token one character
// at a time. It costs nothing to do correctly.
func VerifyToken(received, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		// No token configured means every delivery is refused rather than
		// every delivery accepted. A webhook endpoint that authenticates
		// nothing is an endpoint anybody can post orders to.
		return ErrMissingWebhookToken
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(received)), []byte(expected)) != 1 {
		return ErrInvalidWebhookToken
	}
	return nil
}

// ParseNotification extracts what is worth acting on, and nothing else.
func ParseNotification(raw []byte) (*Notification, error) {
	var notification Notification
	if err := json.Unmarshal(raw, &notification); err != nil {
		return nil, err
	}
	if strings.TrimSpace(notification.Payment.ID) == "" {
		return nil, ErrUnusableNotification
	}
	return &notification, nil
}

// PaymentID is the id to read back from the API.
func (n *Notification) PaymentID() string { return strings.TrimSpace(n.Payment.ID) }

// DeliveryID identifies this delivery for deduplication. Asaas sends an `id` on
// the event envelope; without one the payment id is the next best key.
func (n *Notification) DeliveryID() string {
	if id := strings.TrimSpace(n.ID); id != "" {
		return id
	}
	return n.PaymentID()
}

// Actionable reports whether this event could change an order.
//
// The filter is coarse on purpose. Everything that survives it is re-read from
// the API anyway, so a false positive costs one call; a false negative loses a
// payment. Only events that cannot possibly concern a charge are dropped.
func (n *Notification) Actionable() bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(n.Event)), "PAYMENT_")
}
