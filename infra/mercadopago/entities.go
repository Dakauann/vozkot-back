package mercadopago

import "time"

// Payment method identifiers accepted by POST /v1/payments in Brazil. Mercado
// Pago names the concrete instrument rather than the family, which is why
// boleto is "bolbradesco".
const (
	PaymentMethodPix    = "pix"
	PaymentMethodBoleto = "bolbradesco"
)

// Identification document types recognised by Mercado Pago Brazil.
const (
	IdentificationCPF  = "CPF"
	IdentificationCNPJ = "CNPJ"
)

// CreatePaymentRequest is the body of POST /v1/payments.
//
// Reference: https://www.mercadopago.com/developers/en/reference/payments/_payments/post
type CreatePaymentRequest struct {
	TransactionAmount float64 `json:"transaction_amount"`
	PaymentMethodID   string  `json:"payment_method_id"`
	Description       string  `json:"description,omitempty"`
	// ExternalReference carries our order id. It is what ties a payment back to
	// an order when a notification arrives before the charge response was
	// persisted.
	ExternalReference string `json:"external_reference,omitempty"`
	// NotificationURL overrides the account-level webhook URL for this payment
	// only, which is what guarantees the data.id query parameter the webhook
	// signature is computed over.
	NotificationURL string `json:"notification_url,omitempty"`
	// DateOfExpiration is ISO-8601 with milliseconds and an explicit offset.
	// Mercado Pago rejects every other layout.
	DateOfExpiration string         `json:"date_of_expiration,omitempty"`
	Payer            PayerRequest   `json:"payer"`
	Metadata         map[string]any `json:"metadata,omitempty"`
}

// PayerRequest identifies the person being charged. Email is always required;
// identification is required for PIX in Brazil.
type PayerRequest struct {
	Email          string          `json:"email"`
	FirstName      string          `json:"first_name,omitempty"`
	LastName       string          `json:"last_name,omitempty"`
	Identification *Identification `json:"identification,omitempty"`
}

type Identification struct {
	Type   string `json:"type,omitempty"`
	Number string `json:"number,omitempty"`
}

// Payment is the resource returned by POST/GET /v1/payments. Only the fields
// this integration reads are modelled; Mercado Pago returns many more.
type Payment struct {
	ID                        int64      `json:"id"`
	Status                    string     `json:"status"`
	StatusDetail              string     `json:"status_detail"`
	ExternalReference         string     `json:"external_reference"`
	Description               string     `json:"description"`
	PaymentMethodID           string     `json:"payment_method_id"`
	PaymentTypeID             string     `json:"payment_type_id"`
	CurrencyID                string     `json:"currency_id"`
	TransactionAmount         float64    `json:"transaction_amount"`
	TransactionAmountRefunded float64    `json:"transaction_amount_refunded"`
	LiveMode                  bool       `json:"live_mode"`
	DateCreated               *time.Time `json:"date_created"`
	DateApproved              *time.Time `json:"date_approved"`
	DateOfExpiration          *time.Time `json:"date_of_expiration"`

	PointOfInteraction PointOfInteraction `json:"point_of_interaction"`
	TransactionDetails TransactionDetails `json:"transaction_details"`
}

// PointOfInteraction carries the PIX artifacts: the copy-paste string and the
// base64 QR image.
type PointOfInteraction struct {
	TransactionData struct {
		QRCode       string `json:"qr_code"`
		QRCodeBase64 string `json:"qr_code_base64"`
		TicketURL    string `json:"ticket_url"`
	} `json:"transaction_data"`
}

type TransactionDetails struct {
	ExternalResourceURL string `json:"external_resource_url"`
}

// Refund is the resource returned by POST /v1/payments/{id}/refunds.
type Refund struct {
	ID     int64   `json:"id"`
	Amount float64 `json:"amount"`
	Status string  `json:"status"`
}

// APIError is Mercado Pago's error envelope.
type APIError struct {
	Message string       `json:"message"`
	Error   string       `json:"error"`
	Status  int          `json:"status"`
	Cause   []ErrorCause `json:"cause"`
}

type ErrorCause struct {
	Code        any    `json:"code"`
	Description string `json:"description"`
}
