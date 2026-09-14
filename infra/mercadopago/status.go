package mercadopago

import (
	"strings"

	"vozkot/domain/payment"
)

// Payment statuses returned by /v1/payments.
//
// Reference: https://www.mercadopago.com/developers/en/docs/checkout-api/response-handling/collection-results
const (
	StatusPending     = "pending"
	StatusApproved    = "approved"
	StatusAuthorized  = "authorized"
	StatusInProcess   = "in_process"
	StatusInMediation = "in_mediation"
	StatusRejected    = "rejected"
	StatusCancelled   = "cancelled"
	StatusRefunded    = "refunded"
	StatusChargedBack = "charged_back"
)

// status_detail values this integration reasons about.
const (
	DetailAccredited        = "accredited"
	DetailPartiallyRefunded = "partially_refunded"
	DetailExpired           = "expired"
)

// MapStatus translates a Mercado Pago payment into the canonical status.
//
// It reads the whole payment rather than the status string alone, because two
// cases are invisible from the status: a partially refunded charge stays
// "approved" and only moves status_detail, and an unpaid PIX that ran out its
// clock arrives as "cancelled" with status_detail "expired", which is "not
// paid in time", not "voided by us".
//
// in_process and in_mediation deliberately map to in-analysis: the money is not
// ours yet, and treating a disputed payment as received would sell a ticket
// against funds that may be clawed back.
func MapStatus(item *Payment) payment.Status {
	if item == nil {
		return payment.StatusPending
	}
	status := strings.ToLower(strings.TrimSpace(item.Status))
	detail := strings.ToLower(strings.TrimSpace(item.StatusDetail))

	switch status {
	case StatusApproved:
		if detail == DetailPartiallyRefunded {
			return payment.StatusRefunded
		}
		if item.TransactionAmountRefunded > 0 && item.TransactionAmountRefunded >= item.TransactionAmount {
			return payment.StatusRefunded
		}
		return payment.StatusPaid
	case StatusRefunded:
		return payment.StatusRefunded
	case StatusChargedBack:
		return payment.StatusChargedBack
	case StatusRejected:
		return payment.StatusRejected
	case StatusCancelled:
		return payment.StatusCancelled
	case StatusInProcess, StatusInMediation:
		return payment.StatusInAnalysis
	case StatusAuthorized, StatusPending:
		return payment.StatusPending
	default:
		return payment.StatusPending
	}
}

// MapMethod translates a payment_method_id into the canonical method.
func MapMethod(paymentMethodID string) payment.Method {
	switch strings.ToLower(strings.TrimSpace(paymentMethodID)) {
	case PaymentMethodPix, "":
		return payment.MethodPix
	case PaymentMethodBoleto, "boleto", "pec":
		return payment.MethodBoleto
	default:
		return payment.MethodCard
	}
}

// PaymentMethodIDFor spells a canonical method the way POST /v1/payments wants
// it, and refuses the ones a server-side integration cannot issue.
func PaymentMethodIDFor(method payment.Method) (string, error) {
	switch method {
	case payment.MethodPix, "":
		return PaymentMethodPix, nil
	case payment.MethodBoleto:
		return PaymentMethodBoleto, nil
	default:
		// A card charge needs a token minted by the browser SDK, which this
		// server never holds. Failing loudly beats silently downgrading the
		// buyer to a different instrument.
		return "", payment.ErrMethodUnsupported
	}
}
