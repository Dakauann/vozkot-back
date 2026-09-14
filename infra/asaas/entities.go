// Package asaas adapts Asaas to the provider-agnostic payment port.
//
// Everything Asaas-shaped lives in here: the `access_token` header it uses
// instead of a bearer, the get-or-create customer record every charge needs, the
// separate call that produces a PIX payload, the wallet ids a split is addressed
// to, and, the one that matters most, the fact that Asaas speaks REAIS AS
// FLOATS while the rest of this system speaks integer centavos.
//
// The conversion happens at this boundary and nowhere else. See money.go.
package asaas

// Customer is an Asaas payer record.
//
// Asaas will not accept a charge without one, and it is keyed on the document,
// which is why every charge needs the buyer's CPF and why an empty document has
// to be refused rather than sent.
type Customer struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Document string `json:"cpfCnpj"`
}

// Payment is an Asaas charge, in Asaas's own shape and units.
//
// Value and every other money field is REAIS AS A FLOAT. Nothing outside this
// package should ever see one.
type Payment struct {
	ID          string `json:"id,omitempty"`
	Customer    string `json:"customer"`
	BillingType string `json:"billingType"`
	// Value is in reais. Converted from centavos on the way in and back to
	// centavos on the way out.
	Value float64 `json:"value"`
	// DueDate is a DATE, not an instant: "2026-09-13".
	//
	// This is a genuine semantic gap with how this system works. A hold here
	// lasts thirty minutes; the shortest expiry Asaas accepts is the end of a
	// day. The charge therefore outlives its own reservation, which makes the
	// late-payment path, already implemented, already tested, the normal case
	// rather than the exception. See the note in gateway.go.
	DueDate           string  `json:"dueDate"`
	Description       string  `json:"description,omitempty"`
	ExternalReference string  `json:"externalReference,omitempty"`
	Status            string  `json:"status,omitempty"`
	NetValue          float64 `json:"netValue,omitempty"`
	// Split addresses parts of this charge to other Asaas accounts by wallet
	// id. Unused today; the field is here because it is the whole reason this
	// provider was chosen, and leaving it out would mean rewriting the entity
	// the day it is needed.
	Split []Split `json:"split,omitempty"`

	BankSlipURL string `json:"bankSlipUrl,omitempty"`
	InvoiceURL  string `json:"invoiceUrl,omitempty"`

	DateCreated  string `json:"dateCreated,omitempty"`
	ConfirmedAt  string `json:"confirmedDate,omitempty"`
	PaymentDate  string `json:"paymentDate,omitempty"`
	ClientDate   string `json:"clientPaymentDate,omitempty"`
	Deleted      bool   `json:"deleted,omitempty"`
	PostalMailed bool   `json:"postalService,omitempty"`
}

// Split is one leg of a divided charge, addressed by wallet id.
//
// Exactly one of FixedValue and PercentualValue is meaningful per leg; sending
// both is how a split stops adding up to the charge it divides.
type Split struct {
	WalletID          string  `json:"walletId"`
	FixedValue        float64 `json:"fixedValue,omitempty"`
	PercentualValue   float64 `json:"percentualValue,omitempty"`
	ExternalReference string  `json:"externalReference,omitempty"`
	Description       string  `json:"description,omitempty"`
}

// PixQRCode is the second call a PIX charge needs.
//
// Asaas creates the charge and the payload separately, so a charge can exist
// and be payable through its invoice URL while this call has not succeeded yet.
type PixQRCode struct {
	// EncodedImage is base64 PNG, already in the form an <img src> takes.
	EncodedImage string `json:"encodedImage"`
	// Payload is the copy-and-paste EMV string.
	Payload        string `json:"payload"`
	ExpirationDate string `json:"expirationDate"`
}

// list is the envelope every Asaas collection endpoint returns.
type list[T any] struct {
	Data       []T  `json:"data"`
	HasMore    bool `json:"hasMore"`
	TotalCount int  `json:"totalCount"`
}

// Asaas payment statuses.
//
// Reference: https://docs.asaas.com/reference/status-de-cobrancas
const (
	StatusPending                    = "PENDING"
	StatusReceived                   = "RECEIVED"
	StatusConfirmed                  = "CONFIRMED"
	StatusOverdue                    = "OVERDUE"
	StatusRefunded                   = "REFUNDED"
	StatusRefundRequested            = "REFUND_REQUESTED"
	StatusRefundInProgress           = "REFUND_IN_PROGRESS"
	StatusReceivedInCash             = "RECEIVED_IN_CASH"
	StatusChargebackRequested        = "CHARGEBACK_REQUESTED"
	StatusChargebackDispute          = "CHARGEBACK_DISPUTE"
	StatusAwaitingChargebackReversal = "AWAITING_CHARGEBACK_REVERSAL"
	StatusAwaitingRiskAnalysis       = "AWAITING_RISK_ANALYSIS"
	StatusDunningRequested           = "DUNNING_REQUESTED"
	StatusDunningReceived            = "DUNNING_RECEIVED"
)

// Billing types.
const (
	BillingPix        = "PIX"
	BillingBoleto     = "BOLETO"
	BillingCreditCard = "CREDIT_CARD"
	BillingUndefined  = "UNDEFINED"
)
