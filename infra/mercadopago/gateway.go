package mercadopago

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"vozkot/domain/payment"
)

// gateway adapts the Mercado Pago client to the provider-agnostic port.
//
// Every Mercado-Pago-shaped concern stops here: the payer identification model,
// the inline PIX payload, the ISO-8601 expiration layout, the fact that amounts
// cross the wire as decimals rather than cents.
type gateway struct {
	client            Client
	now               func() time.Time
	sandboxPayerEmail string
}

type GatewayOption func(*gateway)

// WithClock overrides the clock used for expiry clamping. For tests.
func WithClock(now func() time.Time) GatewayOption {
	return func(g *gateway) {
		if now != nil {
			g.now = now
		}
	}
}

// WithSandboxPayerEmail addresses every charge to a fixed payer instead of the
// real buyer.
//
// Mercado Pago's sandbox rejects a charge whose payer is not one of its own
// test users, so without this there is no way to drive the real flow end to end
// against sandbox credentials. Configuration refuses it outside development,
// because in production it would put someone else's address on a real charge.
func WithSandboxPayerEmail(email string) GatewayOption {
	return func(g *gateway) { g.sandboxPayerEmail = strings.TrimSpace(email) }
}

func NewGateway(client Client, opts ...GatewayOption) payment.Gateway {
	adapter := &gateway{client: client, now: time.Now}
	for _, opt := range opts {
		opt(adapter)
	}
	return adapter
}

func (g *gateway) Provider() payment.Provider { return payment.ProviderMercadoPago }

func (g *gateway) CreateCharge(ctx context.Context, request payment.ChargeRequest) (*payment.Charge, error) {
	if g == nil || g.client == nil {
		return nil, payment.ErrNotConfigured
	}
	if request.AmountCents <= 0 {
		return nil, payment.ErrInvalidAmount
	}
	email := strings.TrimSpace(request.Customer.Email)
	if email == "" {
		return nil, payment.ErrEmailRequired
	}
	document := onlyDigits(request.Customer.Document)
	if document == "" {
		// PIX in Brazil is issued against a CPF or CNPJ. Mercado Pago rejects
		// the charge without one, so it is refused here with an error a caller
		// can act on rather than as an opaque 400 from the provider.
		return nil, payment.ErrDocumentRequired
	}
	methodID, err := PaymentMethodIDFor(request.Method)
	if err != nil {
		return nil, err
	}

	if g.sandboxPayerEmail != "" {
		email = g.sandboxPayerEmail
	}

	firstName, lastName := splitName(request.Customer.Name)
	// Mercado Pago's minimum PIX window is 30 minutes. A shorter hold is
	// clamped up, which means a code can outlive its reservation, the case the
	// order's late-payment path exists to settle honestly.
	expiresAt := ClampExpiry(request.ExpiresAt, g.now())

	payload := CreatePaymentRequest{
		TransactionAmount: centsToAmount(request.AmountCents),
		PaymentMethodID:   methodID,
		Description:       strings.TrimSpace(request.Description),
		ExternalReference: strings.TrimSpace(request.ExternalReference),
		DateOfExpiration:  expiresAt.Format(ExpirationLayout),
		Payer: PayerRequest{
			Email:     email,
			FirstName: firstName,
			LastName:  lastName,
			Identification: &Identification{
				Type:   identificationTypeFor(document),
				Number: document,
			},
		},
		Metadata: map[string]any{"order_id": request.ExternalReference},
	}

	created, err := g.client.CreatePayment(ctx, payload, request.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return toCharge(created), nil
}

func (g *gateway) GetCharge(ctx context.Context, chargeID string) (*payment.Charge, error) {
	if g == nil || g.client == nil {
		return nil, payment.ErrNotConfigured
	}
	found, err := g.client.GetPayment(ctx, chargeID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, payment.ErrChargeNotFound
		}
		return nil, err
	}
	return toCharge(found), nil
}

func (g *gateway) CancelCharge(ctx context.Context, chargeID string) error {
	if g == nil || g.client == nil {
		return payment.ErrNotConfigured
	}
	_, err := g.client.CancelPayment(ctx, chargeID)
	if errors.Is(err, ErrNotFound) {
		return payment.ErrChargeNotFound
	}
	return err
}

func (g *gateway) RefundCharge(ctx context.Context, chargeID string, amountCents int64) error {
	if g == nil || g.client == nil {
		return payment.ErrNotConfigured
	}
	amount := 0.0
	if amountCents > 0 {
		amount = centsToAmount(amountCents)
	}
	// The refund is keyed on the charge, so a retried refund job refunds once.
	_, err := g.client.RefundPayment(ctx, chargeID, amount, "refund:"+chargeID)
	if errors.Is(err, ErrNotFound) {
		return payment.ErrChargeNotFound
	}
	return err
}

func toCharge(item *Payment) *payment.Charge {
	if item == nil {
		return nil
	}
	charge := &payment.Charge{
		ID:                   strconv.FormatInt(item.ID, 10),
		Provider:             payment.ProviderMercadoPago,
		Status:               MapStatus(item),
		Method:               MapMethod(item.PaymentMethodID),
		AmountCents:          amountToCents(item.TransactionAmount),
		AmountRefundedCents:  amountToCents(item.TransactionAmountRefunded),
		ExternalReference:    strings.TrimSpace(item.ExternalReference),
		PixCopyPaste:         item.PointOfInteraction.TransactionData.QRCode,
		PixQRCodeBase64:      item.PointOfInteraction.TransactionData.QRCodeBase64,
		TicketURL:            firstNonEmpty(item.PointOfInteraction.TransactionData.TicketURL, item.TransactionDetails.ExternalResourceURL),
		ExpiresAt:            item.DateOfExpiration,
		PaidAt:               item.DateApproved,
		ProviderStatus:       item.Status,
		ProviderStatusDetail: item.StatusDetail,
	}
	return charge
}

// centsToAmount converts integer cents to the decimal Mercado Pago expects.
// Division is the only place a float appears, and it happens at the wire.
func centsToAmount(cents int64) float64 {
	return float64(cents) / 100
}

// amountToCents converts back, rounding rather than truncating: 24.99 arrives
// from JSON as 24.989999999999998, and truncation would lose a cent.
func amountToCents(amount float64) int64 {
	return int64(math.Round(amount * 100))
}

// identificationTypeFor picks CPF or CNPJ by length, which is how every
// Brazilian payment API distinguishes them.
func identificationTypeFor(document string) string {
	if len(document) > 11 {
		return IdentificationCNPJ
	}
	return IdentificationCPF
}

func splitName(name string) (string, string) {
	fields := strings.Fields(strings.TrimSpace(name))
	switch len(fields) {
	case 0:
		return "", ""
	case 1:
		return fields[0], ""
	default:
		return fields[0], strings.Join(fields[1:], " ")
	}
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

// OnlyDigits is exported for the HTTP layer, which validates a CPF before it
// ever reaches a charge.
func OnlyDigits(value string) string { return onlyDigits(value) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
