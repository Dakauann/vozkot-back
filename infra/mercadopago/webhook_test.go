package mercadopago

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"vozkot/domain/payment"
)

const secret = "a-webhook-secret"

func TestVerifySignatureAcceptsWhatMercadoPagoSends(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	header := SignForTesting("1234567890", "req-1", secret, now)

	if err := VerifySignature(header, "req-1", "1234567890", secret, 0, now); err != nil {
		t.Fatalf("VerifySignature() error = %v", err)
	}
}

func TestVerifySignatureRejectsForgeriesAndMistakes(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	valid := SignForTesting("1234567890", "req-1", secret, now)

	cases := map[string]struct {
		header    string
		requestID string
		dataID    string
		secret    string
		want      SignatureReason
	}{
		"a different payment id": {valid, "req-1", "999", secret, ReasonSignatureMismatch},
		"a different request id": {valid, "req-2", "1234567890", secret, ReasonSignatureMismatch},
		"a different secret":     {valid, "req-1", "1234567890", "not-the-secret", ReasonSignatureMismatch},
		"no signature":           {"", "req-1", "1234567890", secret, ReasonMissingSignatureHeader},
		"no hash":                {"ts=1757764800000", "req-1", "1234567890", secret, ReasonMissingHash},
		"no timestamp":           {"v1=abc123", "req-1", "1234567890", secret, ReasonMissingTimestamp},
		"nonsense":               {"garbage", "req-1", "1234567890", secret, ReasonMalformedSignature},
		// The endpoint must fail CLOSED: an unconfigured secret cannot turn it
		// into an open one anybody may use to mark orders paid.
		"no configured secret": {valid, "req-1", "1234567890", "", ReasonMissingSecret},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			err := VerifySignature(testCase.header, testCase.requestID, testCase.dataID, testCase.secret, 0, now)

			if !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("VerifySignature() error = %v, want an invalid-signature error", err)
			}
			var signatureErr *SignatureError
			if errors.As(err, &signatureErr) && signatureErr.Reason != testCase.want {
				t.Fatalf("reason = %q, want %q", signatureErr.Reason, testCase.want)
			}
		})
	}
}

func TestVerifySignatureToleranceIsOffByDefault(t *testing.T) {
	signedAt := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	header := SignForTesting("1234567890", "req-1", secret, signedAt)
	muchLater := signedAt.Add(6 * time.Hour)

	// Mercado Pago retries a failed delivery for hours WITHOUT re-signing it.
	// With the window off, that retry is still accepted.
	if err := VerifySignature(header, "req-1", "1234567890", secret, 0, muchLater); err != nil {
		t.Fatalf("VerifySignature() with no tolerance error = %v", err)
	}

	// With a window configured, the same delivery is refused.
	err := VerifySignature(header, "req-1", "1234567890", secret, 5*time.Minute, muchLater)
	var signatureErr *SignatureError
	if !errors.As(err, &signatureErr) || signatureErr.Reason != ReasonTimestampOutOfTolerance {
		t.Fatalf("VerifySignature() error = %v, want a tolerance rejection", err)
	}
}

func TestVerifySignatureAnyReportsWhichIDWasSigned(t *testing.T) {
	now := time.Now()
	header := SignForTesting("1234567890", "req-1", secret, now)

	// Some deliveries carry no data.id query parameter while still being signed
	// over the id in the body. Offering both candidates recovers those without
	// weakening anything: each candidate is a full HMAC comparison.
	matched, err := VerifySignatureAny(header, "req-1", []string{"", "1234567890"}, secret, 0, now)

	if err != nil {
		t.Fatalf("VerifySignatureAny() error = %v", err)
	}
	if matched != "1234567890" {
		t.Fatalf("matched id = %q, want the one that was actually signed", matched)
	}
}

func TestParseNotificationReadsBothEnvelopeFormats(t *testing.T) {
	t.Run("modern webhook", func(t *testing.T) {
		body := []byte(`{"id":112233,"type":"payment","action":"payment.updated","data":{"id":"1234567890"}}`)

		notification, err := ParseNotification(body, nil)

		if err != nil {
			t.Fatalf("ParseNotification() error = %v", err)
		}
		if !notification.IsPayment() || notification.ResourceID() != "1234567890" {
			t.Fatalf("parsed = %+v", notification)
		}
		if notification.DeliveryID() != "112233" {
			t.Fatalf("delivery id = %q, want the notification id", notification.DeliveryID())
		}
	})

	t.Run("legacy IPN with no body", func(t *testing.T) {
		query := url.Values{"topic": {"payment"}, "id": {"1234567890"}}

		notification, err := ParseNotification(nil, query)

		if err != nil {
			t.Fatalf("ParseNotification() error = %v", err)
		}
		if !notification.IsPayment() || notification.ResourceID() != "1234567890" {
			t.Fatalf("parsed = %+v", notification)
		}
	})

	t.Run("legacy resource URL", func(t *testing.T) {
		body := []byte(`{"topic":"payment","resource":"https://api.mercadolibre.com/collections/notifications/998877"}`)

		notification, err := ParseNotification(body, nil)

		if err != nil {
			t.Fatalf("ParseNotification() error = %v", err)
		}
		if notification.ResourceID() != "998877" {
			t.Fatalf("resource id = %q, want the trailing id of the URL", notification.ResourceID())
		}
	})

	t.Run("nothing to act on", func(t *testing.T) {
		if _, err := ParseNotification([]byte(`{"type":"payment"}`), nil); !errors.Is(err, ErrNotificationMissingResourceID) {
			t.Fatalf("ParseNotification() error = %v, want %v", err, ErrNotificationMissingResourceID)
		}
	})
}

func TestMapStatusCoversEveryStateThatMatters(t *testing.T) {
	cases := map[string]struct {
		payment Payment
		want    payment.Status
	}{
		"approved":             {Payment{Status: StatusApproved, StatusDetail: DetailAccredited}, payment.StatusPaid},
		"rejected":             {Payment{Status: StatusRejected}, payment.StatusRejected},
		"cancelled":            {Payment{Status: StatusCancelled}, payment.StatusCancelled},
		"expired pix":          {Payment{Status: StatusCancelled, StatusDetail: DetailExpired}, payment.StatusCancelled},
		"refunded":             {Payment{Status: StatusRefunded}, payment.StatusRefunded},
		"charged back":         {Payment{Status: StatusChargedBack}, payment.StatusChargedBack},
		"under review":         {Payment{Status: StatusInProcess}, payment.StatusInAnalysis},
		"in mediation":         {Payment{Status: StatusInMediation}, payment.StatusInAnalysis},
		"waiting for transfer": {Payment{Status: StatusPending}, payment.StatusPending},
		// A partial refund leaves the payment approved and only moves the
		// detail; reading the status alone would keep it "paid" forever.
		"partially refunded": {
			Payment{Status: StatusApproved, StatusDetail: DetailPartiallyRefunded, TransactionAmount: 100, TransactionAmountRefunded: 40},
			payment.StatusRefunded,
		},
		"fully refunded while approved": {
			Payment{Status: StatusApproved, TransactionAmount: 100, TransactionAmountRefunded: 100},
			payment.StatusRefunded,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			item := testCase.payment
			if got := MapStatus(&item); got != testCase.want {
				t.Fatalf("MapStatus() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestPaymentMethodIDRefusesWhatAServerCannotIssue(t *testing.T) {
	if id, err := PaymentMethodIDFor(payment.MethodPix); err != nil || id != PaymentMethodPix {
		t.Fatalf("PaymentMethodIDFor(pix) = (%q, %v)", id, err)
	}
	// A card charge needs a token minted by the browser SDK. Failing loudly
	// beats silently downgrading the buyer to another instrument.
	if _, err := PaymentMethodIDFor(payment.MethodCard); !errors.Is(err, payment.ErrMethodUnsupported) {
		t.Fatalf("PaymentMethodIDFor(card) error = %v, want %v", err, payment.ErrMethodUnsupported)
	}
}
