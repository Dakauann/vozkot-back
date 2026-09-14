package asaas

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"vozkot/domain/payment"
)

// Gateway adapts Asaas to payment.Gateway.
//
// Two Asaas facts shape everything below, and both are handled here so nothing
// above this file has to know them:
//
//  1. A charge needs a CUSTOMER RECORD, resolved from the buyer's document. Asaas
//     will not bill an anonymous payer.
//  2. There is NO IDEMPOTENCY HEADER. A retried charge job could otherwise open a
//     second charge for one order: two PIX codes, two payable amounts, one set
//     of tickets. `externalReference` carries our order id and is searched before
//     anything is created, which is how this provider does idempotency.
type Gateway struct {
	client *Client
	// dueDays is how many days out a charge's due date is set.
	//
	// Asaas dates a charge to a DAY; this system holds stock for thirty minutes.
	// The charge therefore outlives its reservation no matter what is chosen
	// here, which makes the late-payment path; pay after the hold lapsed, stock
	// re-taken if still there, refund owed if not, the ordinary case rather
	// than the exception. It is already implemented and already tested; this
	// comment exists so the next person does not read the frequency as a bug.
	//
	// One day, not seven: the shorter it is, the sooner an abandoned charge
	// stops being payable, and a buyer paying a two-day-old PIX for an event
	// that sold out in between is the case nobody wants to handle.
	dueDays int
	now     func() time.Time
}

type GatewayOption func(*Gateway)

// WithDueDays overrides how far out a charge is dated.
func WithDueDays(days int) GatewayOption {
	return func(g *Gateway) {
		if days > 0 {
			g.dueDays = days
		}
	}
}

func NewGateway(client *Client, options ...GatewayOption) *Gateway {
	gateway := &Gateway{client: client, dueDays: 1, now: time.Now}
	for _, apply := range options {
		apply(gateway)
	}
	return gateway
}

var _ payment.Gateway = (*Gateway)(nil)

func (g *Gateway) Provider() payment.Provider { return payment.ProviderAsaas }

// CreateCharge issues a charge, or returns the one this order already has.
func (g *Gateway) CreateCharge(ctx context.Context, request payment.ChargeRequest) (*payment.Charge, error) {
	if g == nil || !g.client.Configured() {
		return nil, payment.ErrNotConfigured
	}
	if request.AmountCents <= 0 {
		return nil, payment.ErrInvalidAmount
	}
	if strings.TrimSpace(request.Customer.Document) == "" {
		return nil, payment.ErrDocumentRequired
	}
	method := request.Method
	if method == "" {
		method = payment.MethodPix
	}

	// Idempotency, done the only way this provider allows. A job that timed out
	// after Asaas had already created the charge finds it here instead of
	// opening a second one.
	if existing, err := g.client.FindPaymentByReference(ctx, request.ExternalReference); err != nil {
		return nil, err
	} else if existing != nil {
		return g.complete(ctx, existing), nil
	}

	customer, err := g.resolveCustomer(ctx, request.Customer)
	if err != nil {
		return nil, err
	}

	created, err := g.client.CreatePayment(ctx, Payment{
		Customer:          customer.ID,
		BillingType:       BillingTypeFor(method),
		Value:             toReais(request.AmountCents),
		DueDate:           g.dueDate(request.ExpiresAt),
		Description:       request.Description,
		ExternalReference: request.ExternalReference,
	})
	if err != nil {
		return nil, err
	}
	return g.complete(ctx, created), nil
}

func (g *Gateway) GetCharge(ctx context.Context, chargeID string) (*payment.Charge, error) {
	if g == nil || !g.client.Configured() {
		return nil, payment.ErrNotConfigured
	}
	found, err := g.client.GetPayment(ctx, chargeID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, payment.ErrChargeNotFound
		}
		return nil, err
	}
	// No QR fetch on a read: the payload is already stored on the order, and
	// re-fetching it on every reconciliation sweep would double the calls this
	// system makes for a value that never changes.
	return g.toCharge(found), nil
}

func (g *Gateway) CancelCharge(ctx context.Context, chargeID string) error {
	if g == nil || !g.client.Configured() {
		return payment.ErrNotConfigured
	}
	err := g.client.DeletePayment(ctx, chargeID)
	if errors.Is(err, ErrNotFound) {
		// Already gone. Cancelling a charge that does not exist is the outcome
		// the caller wanted.
		return nil
	}
	return err
}

func (g *Gateway) RefundCharge(ctx context.Context, chargeID string, amountCents int64) error {
	if g == nil || !g.client.Configured() {
		return payment.ErrNotConfigured
	}
	// A non-positive amount means "all of it", which is what the port
	// documents and what Asaas does with a missing value.
	value := 0.0
	if amountCents > 0 {
		value = toReais(amountCents)
	}
	return g.client.RefundPayment(ctx, chargeID, value, "Reembolso do pedido")
}

// resolveCustomer finds or creates the Asaas payer record for this buyer.
func (g *Gateway) resolveCustomer(ctx context.Context, buyer payment.Customer) (*Customer, error) {
	found, err := g.client.FindCustomerByDocument(ctx, buyer.Document)
	if err != nil {
		if errors.Is(err, ErrDocumentMissing) {
			return nil, payment.ErrDocumentRequired
		}
		return nil, err
	}
	if found != nil {
		return found, nil
	}
	return g.client.CreateCustomer(ctx, Customer{
		Name:     strings.TrimSpace(buyer.Name),
		Email:    strings.TrimSpace(buyer.Email),
		Document: buyer.Document,
	})
}

// complete fills in the PIX payload, which Asaas serves from its own endpoint.
//
// A failure here is deliberately NOT fatal. The charge exists and is payable;
// losing the QR code costs this attempt its copy-and-paste string, and the
// order's poll picks it up on the next sync rather than the buyer losing a
// reservation over a second HTTP call.
func (g *Gateway) complete(ctx context.Context, item *Payment) *payment.Charge {
	charge := g.toCharge(item)
	if charge.Method != payment.MethodPix || item.ID == "" {
		return charge
	}
	// Only for a charge somebody can still pay. Asking for the QR of a refunded
	// charge is a call that can only fail.
	if charge.Status != payment.StatusPending {
		return charge
	}

	code, err := g.client.GetPixQRCode(ctx, item.ID)
	if err != nil {
		log.Printf("asaas: charge %s created but the PIX payload could not be fetched: %v", item.ID, err)
		return charge
	}
	charge.PixQRCodeBase64 = code.EncodedImage
	charge.PixCopyPaste = code.Payload
	return charge
}

func (g *Gateway) toCharge(item *Payment) *payment.Charge {
	charge := &payment.Charge{
		ID:                item.ID,
		Provider:          payment.ProviderAsaas,
		Status:            MapStatus(item.Status),
		Method:            MapMethod(item.BillingType),
		AmountCents:       toCents(item.Value),
		ExternalReference: item.ExternalReference,
		TicketURL:         item.InvoiceURL,
		ProviderStatus:    item.Status,
	}
	if item.BankSlipURL != "" {
		charge.TicketURL = item.BankSlipURL
	}
	// A deleted charge is void whatever its status said a moment ago.
	if item.Deleted && charge.Status == payment.StatusPending {
		charge.Status = payment.StatusCancelled
	}
	if due, err := time.Parse("2006-01-02", item.DueDate); err == nil {
		// End of the due DAY, because that is when Asaas stops accepting it,
		// not the midnight that starts it.
		expires := due.Add(24*time.Hour - time.Second)
		charge.ExpiresAt = &expires
	}
	if paid := firstDate(item.PaymentDate, item.ClientDate, item.ConfirmedAt); paid != nil {
		charge.PaidAt = paid
	}
	return charge
}

// dueDate dates the charge, never in the past.
func (g *Gateway) dueDate(holdExpiry time.Time) string {
	day := g.now().UTC().AddDate(0, 0, g.dueDays)
	// A hold that somehow runs past the default window extends the charge to
	// match, so a buyer is never handed a code that expires before their own
	// reservation does.
	if !holdExpiry.IsZero() && holdExpiry.After(day) {
		day = holdExpiry.UTC()
	}
	return day.Format("2006-01-02")
}

func firstDate(candidates ...string) *time.Time {
	for _, raw := range candidates {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
			if parsed, err := time.Parse(layout, raw); err == nil {
				return &parsed
			}
		}
	}
	return nil
}
