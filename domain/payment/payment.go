// Package payment is the provider-agnostic view of money movement.
//
// Nothing here names Mercado Pago. The gateway port below is what an adapter
// implements, so a second provider is a new package under infra and not a
// change to a use case.
package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Provider identifies the payment processor behind a charge.
type Provider string

const (
	// ProviderAsaas is the default. It is the one that can divide a charge
	// across wallet ids, which is what a box office paying organisers needs.
	ProviderAsaas       Provider = "asaas"
	ProviderMercadoPago Provider = "mercadopago"
)

// ParseProvider normalises an operator-supplied provider name.
//
// Empty means Asaas: a deployment that says nothing gets the provider this
// system is built around, rather than failing to start over a variable nobody
// knew to set.
func ParseProvider(raw string) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(strings.ReplaceAll(raw, " ", ""))) {
	case "", string(ProviderAsaas):
		return ProviderAsaas, nil
	case string(ProviderMercadoPago), "mercado_pago", "mercado-pago", "mp":
		return ProviderMercadoPago, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownProvider, raw)
	}
}

// Method is the instrument the buyer pays with.
//
// PIX is the only one this box office issues server-side: a card charge needs a
// token minted by the provider's browser SDK, which a server integration never
// holds, and issuing boleto for an event that starts tomorrow sells a ticket
// that cannot be paid in time.
type Method string

const (
	MethodPix    Method = "pix"
	MethodBoleto Method = "boleto"
	MethodCard   Method = "card"
)

// Status is the canonical charge state, translated from whatever vocabulary the
// provider uses.
type Status string

const (
	StatusPending   Status = "pending"
	StatusPaid      Status = "paid"
	StatusRejected  Status = "rejected"
	StatusCancelled Status = "cancelled"
	StatusRefunded  Status = "refunded"
	// StatusChargedBack is money already received being pulled back. It is kept
	// distinct from a refund because the two need different accounting even
	// though both return the ticket to stock.
	StatusChargedBack Status = "charged_back"
	// StatusInAnalysis is under review by the provider: informational, and
	// deliberately moves nothing.
	StatusInAnalysis Status = "in_analysis"
)

// Final reports whether a status can still change on its own.
func (s Status) Final() bool {
	switch s {
	case StatusPaid, StatusRejected, StatusCancelled, StatusRefunded, StatusChargedBack:
		return true
	default:
		return false
	}
}

var (
	ErrInvalidAmount     = errors.New("charge amount must be positive")
	ErrMethodUnsupported = errors.New("payment provider does not support this method")
	ErrChargeNotFound    = errors.New("charge not found at payment provider")
	ErrEmailRequired     = errors.New("buyer email is required to create a charge")
	ErrDocumentRequired  = errors.New("buyer CPF/CNPJ is required to create a charge")
	ErrNotConfigured     = errors.New("payment provider is not configured")
	ErrUnknownProvider   = errors.New("unknown payment provider")
)

// retryable is implemented by a provider error that knows whether repeating the
// request could plausibly succeed.
type retryable interface{ Retryable() bool }

// Retryable reports whether repeating a provider call is worth doing.
//
// The adapter is the only thing that can answer it, a 502 deserves another
// attempt, a rejected request does not, so the question is asked through a
// tiny interface rather than by reading status codes up here. The knowledge
// stays with the provider; the use case stays free of its vocabulary.
//
// An unrecognised error is treated as transient on purpose. Giving up on a
// payment that would have settled costs a buyer the tickets they paid for;
// one more attempt costs a round trip. The failures that no retry can fix are
// the ones that have to say so.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var known retryable
	if errors.As(err, &known) {
		return known.Retryable()
	}
	switch {
	case errors.Is(err, ErrNotConfigured),
		errors.Is(err, ErrMethodUnsupported),
		errors.Is(err, ErrInvalidAmount),
		errors.Is(err, ErrEmailRequired),
		errors.Is(err, ErrDocumentRequired),
		errors.Is(err, ErrChargeNotFound):
		return false
	}
	return true
}

// Customer is the payer as the provider needs to see them.
type Customer struct {
	Name     string
	Email    string
	Document string
}

// ChargeRequest asks a provider for money.
//
// The amount is in CENTS, like everywhere else in this system. The adapter
// converts to whatever the provider's wire format wants; no float reaches a
// use case, where two of them added together stop agreeing with the order.
type ChargeRequest struct {
	Method      Method
	AmountCents int64
	Currency    string
	Description string
	// ExternalReference is the order id. It is what lets a webhook, a
	// reconciliation sweep or a support ticket tie provider state back to ours.
	ExternalReference string
	ExpiresAt         time.Time
	Customer          Customer
	// IdempotencyKey is forwarded to providers that accept one, so a retried
	// job cannot create a second charge for the same order.
	IdempotencyKey string
}

// Charge is an issued charge, as the provider currently sees it.
type Charge struct {
	ID          string
	Provider    Provider
	Status      Status
	Method      Method
	AmountCents int64
	// AmountRefundedCents is what has been given back so far; a partial refund
	// leaves a charge paid with a non-zero value here.
	AmountRefundedCents int64
	ExternalReference   string
	// PixCopyPaste and PixQRCodeBase64 are how a buyer actually pays.
	PixCopyPaste    string
	PixQRCodeBase64 string
	TicketURL       string
	ExpiresAt       *time.Time
	PaidAt          *time.Time
	// ProviderStatus and ProviderStatusDetail carry the untranslated values for
	// logs and support. Never branch business logic on them outside an adapter.
	ProviderStatus       string
	ProviderStatusDetail string
}

// Gateway is the port every provider adapter implements.
type Gateway interface {
	Provider() Provider
	// CreateCharge issues a charge, fully populated: for PIX that includes the
	// copy-paste payload, fetched by the adapter if the provider needs a second
	// call to produce it.
	CreateCharge(ctx context.Context, request ChargeRequest) (*Charge, error)
	// GetCharge is the source of truth. A webhook says only that something
	// happened; what it became is always read back from the provider.
	GetCharge(ctx context.Context, chargeID string) (*Charge, error)
	CancelCharge(ctx context.Context, chargeID string) error
	RefundCharge(ctx context.Context, chargeID string, amountCents int64) error
}

// Notification is a provider webhook reduced to the only two things a webhook
// is trusted for: which charge moved, and which delivery said so.
//
// The state itself is never taken from the notification body. A forged or stale
// payload could otherwise mark an order paid; reading the charge back from the
// provider cannot.
type Notification struct {
	Provider Provider
	// ChargeID is the provider-side id to read back.
	ChargeID string
	// EventID identifies this delivery, for deduplication. Providers redeliver
	// aggressively, and the same event arriving twice must do the work once.
	EventID string
	Type    string
	Raw     []byte
}

// WebhookVerifier authenticates a raw webhook request before anything acts on
// it. An adapter returning an error must cause the request to be rejected.
type WebhookVerifier interface {
	Verify(signature, requestID, dataID string, now time.Time) error
}
