package asaas

import (
	"strings"

	"vozkot/domain/payment"
)

// MapStatus translates an Asaas payment status into the canonical one.
//
// Three of these decisions move money or stock and are worth stating rather than
// reading off the table:
//
//   - CONFIRMED and RECEIVED both mean PAID. Asaas distinguishes "the provider
//     confirmed it" from "the money is in the account", which matters for a
//     card's settlement cycle and not at all for PIX, where the two are seconds
//     apart. A ticket held until the funds clear is a ticket the buyer cannot
//     use at a door they are already standing at.
//   - A chargeback that has merely been OPENED moves nothing. Treating a
//     disputed payment as clawed back would return a seat to stock while the
//     buyer still holds a valid ticket for it, and most disputes are resolved in
//     the merchant's favour. Only AWAITING_CHARGEBACK_REVERSAL, where the money
//     is actually gone, maps to charged back.
//   - OVERDUE is an unpaid charge past its date, which is the same outcome as a
//     PIX that was never paid: not a failure, just not paid. It maps to
//     cancelled, which order.StatusFor turns into an expired order.
func MapStatus(status string) payment.Status {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case StatusConfirmed, StatusReceived, StatusReceivedInCash, StatusDunningReceived:
		return payment.StatusPaid

	case StatusRefunded, StatusRefundInProgress:
		return payment.StatusRefunded

	case StatusAwaitingChargebackReversal:
		return payment.StatusChargedBack

	case StatusOverdue:
		return payment.StatusCancelled

	case StatusRefundRequested,
		StatusChargebackRequested,
		StatusChargebackDispute,
		StatusAwaitingRiskAnalysis,
		StatusDunningRequested:
		// Under review. Deliberately moves nothing: the money is neither
		// certainly ours nor certainly gone.
		return payment.StatusInAnalysis

	default:
		return payment.StatusPending
	}
}

// MapMethod translates an Asaas billing type into the canonical method.
func MapMethod(billingType string) payment.Method {
	switch strings.ToUpper(strings.TrimSpace(billingType)) {
	case BillingBoleto:
		return payment.MethodBoleto
	case BillingCreditCard:
		return payment.MethodCard
	default:
		return payment.MethodPix
	}
}

// BillingTypeFor spells a canonical method the way Asaas expects it.
func BillingTypeFor(method payment.Method) string {
	switch method {
	case payment.MethodBoleto:
		return BillingBoleto
	case payment.MethodCard:
		return BillingCreditCard
	default:
		return BillingPix
	}
}
