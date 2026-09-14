package asaas

import (
	"errors"
	"testing"

	orderdomain "vozkot/domain/order"
	"vozkot/domain/payment"
)

func TestMapStatusMovesMoneyOnlyWhenItHasMoved(t *testing.T) {
	cases := map[string]payment.Status{
		// Paid. CONFIRMED and RECEIVED are seconds apart on PIX, and a ticket
		// held until funds clear is a ticket nobody can use at the door.
		StatusConfirmed:       payment.StatusPaid,
		StatusReceived:        payment.StatusPaid,
		StatusReceivedInCash:  payment.StatusPaid,
		StatusDunningReceived: payment.StatusPaid,

		// Money going back.
		StatusRefunded:         payment.StatusRefunded,
		StatusRefundInProgress: payment.StatusRefunded,

		// Money already pulled.
		StatusAwaitingChargebackReversal: payment.StatusChargedBack,

		// Not paid in time. Same outcome as a PIX nobody paid.
		StatusOverdue: payment.StatusCancelled,

		// Under review: deliberately moves nothing.
		StatusRefundRequested:      payment.StatusInAnalysis,
		StatusChargebackRequested:  payment.StatusInAnalysis,
		StatusChargebackDispute:    payment.StatusInAnalysis,
		StatusAwaitingRiskAnalysis: payment.StatusInAnalysis,
		StatusDunningRequested:     payment.StatusInAnalysis,

		StatusPending:               payment.StatusPending,
		"":                          payment.StatusPending,
		"SOMETHING_NEW_ASAAS_ADDED": payment.StatusPending,
	}
	for raw, want := range cases {
		if got := MapStatus(raw); got != want {
			t.Fatalf("MapStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestAnOpenedChargebackDoesNotReturnTheSeat.
//
// A dispute being opened is not money leaving. Treating it as a chargeback would
// put the seat back on sale while the buyer still holds a valid ticket for it,
// and most disputes are resolved in the merchant's favour.
func TestAnOpenedChargebackDoesNotReturnTheSeat(t *testing.T) {
	for _, status := range []string{StatusChargebackRequested, StatusChargebackDispute} {
		mapped := MapStatus(status)
		if mapped == payment.StatusChargedBack || mapped == payment.StatusRefunded {
			t.Fatalf("MapStatus(%q) = %q: an opened dispute must move nothing", status, mapped)
		}
		// order.StatusFor is what actually decides whether an order moves; the
		// assertion is on that rather than on the mapping in isolation.
		if _, moves := orderdomain.StatusFor(mapped); moves {
			t.Fatalf("MapStatus(%q) = %q, which moves an order", status, mapped)
		}
	}
}

func TestBillingTypeRoundTrip(t *testing.T) {
	for _, method := range []payment.Method{payment.MethodPix, payment.MethodBoleto, payment.MethodCard} {
		if got := MapMethod(BillingTypeFor(method)); got != method {
			t.Fatalf("%q round-tripped to %q", method, got)
		}
	}
	// An unknown billing type falls back to PIX, the only method this box
	// office issues server-side.
	if got := MapMethod("SOMETHING_ELSE"); got != payment.MethodPix {
		t.Fatalf("unknown billing type mapped to %q", got)
	}
}

func TestWebhookTokenIsCheckedAndFailsClosed(t *testing.T) {
	if err := VerifyToken("secret", "secret"); err != nil {
		t.Fatalf("a matching token was refused: %v", err)
	}
	if err := VerifyToken(" secret ", "secret"); err != nil {
		t.Fatalf("surrounding whitespace broke the comparison: %v", err)
	}
	if err := VerifyToken("wrong", "secret"); !errors.Is(err, ErrInvalidWebhookToken) {
		t.Fatalf("VerifyToken(wrong) = %v, want %v", err, ErrInvalidWebhookToken)
	}
	// No token configured refuses EVERY delivery. An endpoint that
	// authenticates nothing is one anybody can post orders to.
	if err := VerifyToken("anything", ""); !errors.Is(err, ErrMissingWebhookToken) {
		t.Fatalf("VerifyToken with no expected token = %v, want %v", err, ErrMissingWebhookToken)
	}
	if err := VerifyToken("", ""); !errors.Is(err, ErrMissingWebhookToken) {
		t.Fatalf("an empty pair was accepted")
	}
}

func TestParseNotificationTakesOnlyThePaymentID(t *testing.T) {
	notification, err := ParseNotification([]byte(`{
		"id": "evt_123",
		"event": "PAYMENT_RECEIVED",
		"payment": { "id": "pay_abc", "status": "RECEIVED", "externalReference": "ord_1" }
	}`))

	if err != nil {
		t.Fatalf("ParseNotification() error = %v", err)
	}
	if notification.PaymentID() != "pay_abc" {
		t.Fatalf("payment id = %q", notification.PaymentID())
	}
	if notification.DeliveryID() != "evt_123" {
		t.Fatalf("delivery id = %q", notification.DeliveryID())
	}
	if !notification.Actionable() {
		t.Fatal("a PAYMENT_ event was not actionable")
	}
}

func TestParseNotificationRefusesWhatCannotBeActedOn(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   `{`,
		"no payment": `{"event":"PAYMENT_RECEIVED"}`,
		"empty id":   `{"event":"PAYMENT_RECEIVED","payment":{"id":"  "}}`,
	} {
		if _, err := ParseNotification([]byte(body)); err == nil {
			t.Fatalf("%s: ParseNotification() succeeded, want a refusal", name)
		}
	}
}

// TestDeliveryIDFallsBackToThePayment: Asaas may omit the envelope id, and the
// dedupe key still has to be something.
func TestDeliveryIDFallsBackToThePayment(t *testing.T) {
	notification, err := ParseNotification([]byte(`{"event":"PAYMENT_RECEIVED","payment":{"id":"pay_abc"}}`))
	if err != nil {
		t.Fatalf("ParseNotification() error = %v", err)
	}
	if notification.DeliveryID() != "pay_abc" {
		t.Fatalf("delivery id = %q, want the payment id", notification.DeliveryID())
	}
}

func TestNonPaymentEventsAreNotActionable(t *testing.T) {
	for _, event := range []string{"TRANSFER_CREATED", "INVOICE_UPDATED", ""} {
		notification := &Notification{Event: event}
		if notification.Actionable() {
			t.Fatalf("%q was treated as actionable", event)
		}
	}
}
