package checkout

import (
	"time"

	domain "vozkot/domain/order"
)

// The wire format for purchases. As with tickets, the domain entity carries no
// json tags and this file is the only place the API's field names live.

type CheckoutRequest struct {
	TicketID string `json:"ticketId" example:"tkt_a1b2c3d4"`
	Quantity int    `json:"quantity" example:"2"`
	// Method is optional and defaults to PIX, the only instrument this
	// integration can issue from a server.
	Method string `json:"method,omitempty" enums:"pix" example:"pix"`
	Buyer  Buyer  `json:"buyer"`
}

type Buyer struct {
	Name string `json:"name" example:"Maria Souza"`
	// Email receives the ingresso and identifies the payer at Mercado Pago.
	Email string `json:"email" example:"maria@exemplo.com.br"`
	// Document is the CPF or CNPJ. PIX in Brazil cannot be issued without one.
	Document string `json:"document" example:"12345678909"`
}

type CancelRequest struct {
	Reason string `json:"reason,omitempty" example:"Desisti da compra"`
}

type OrderResponse struct {
	ID       string `json:"id" example:"ord_9f2c1d8a"`
	TicketID string `json:"ticketId" example:"tkt_a1b2c3d4"`
	Quantity int    `json:"quantity" example:"2"`

	BuyerName  string `json:"buyerName" example:"Maria Souza"`
	BuyerEmail string `json:"buyerEmail" example:"maria@exemplo.com.br"`

	UnitPriceCents int64  `json:"unitPriceCents" example:"24000"`
	TotalCents     int64  `json:"totalCents" example:"48000"`
	Currency       string `json:"currency" example:"BRL"`

	Status string `json:"status" enums:"pending_payment,paid,expired,cancelled,failed,refunded,refund_required" example:"pending_payment"`
	// HoldExpiresAt is when the reserved tickets go back on sale. It is the
	// clock a checkout screen counts down.
	HoldExpiresAt time.Time `json:"holdExpiresAt"`

	Payment PaymentResponse `json:"payment"`

	PaidAt    *time.Time `json:"paidAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// PaymentResponse is what a buyer needs in order to pay.
//
// It is empty until the queued job has created the charge, which is why the
// client polls: `status` is "pending" and `pixCopyPaste` is blank for the first
// moment of an order's life.
type PaymentResponse struct {
	Provider string `json:"provider" example:"mercadopago"`
	ID       string `json:"id,omitempty" example:"1234567890"`
	Status   string `json:"status" enums:"pending,paid,rejected,cancelled,refunded,charged_back,in_analysis" example:"pending"`
	Method   string `json:"method" example:"pix"`
	// PixCopyPaste is the EMV string a bank app reads.
	PixCopyPaste string `json:"pixCopyPaste,omitempty"`
	// PixQRCodeBase64 is the same payload as a PNG, ready for an <img> tag.
	PixQRCodeBase64 string `json:"pixQrCodeBase64,omitempty"`
}

type OrderEnvelope struct {
	Data OrderResponse `json:"data"`
}

type OrderListEnvelope struct {
	Data   []OrderResponse `json:"data"`
	Total  int64           `json:"total" example:"12"`
	Limit  int             `json:"limit" example:"20"`
	Offset int             `json:"offset" example:"0"`
}

type ErrorResponse struct {
	Error string `json:"error" example:"not enough tickets available"`
}

func toOrderResponse(item *domain.Order) OrderResponse {
	return OrderResponse{
		ID:             item.ID,
		TicketID:       item.TicketID,
		Quantity:       item.Quantity,
		BuyerName:      item.BuyerName,
		BuyerEmail:     item.BuyerEmail,
		UnitPriceCents: item.UnitPriceCents,
		TotalCents:     item.TotalCents,
		Currency:       item.Currency,
		Status:         string(item.Status),
		HoldExpiresAt:  item.HoldExpiresAt,
		Payment: PaymentResponse{
			Provider:        string(item.PaymentProvider),
			ID:              item.PaymentID,
			Status:          string(item.PaymentStatus),
			Method:          string(item.PaymentMethod),
			PixCopyPaste:    item.PixCopyPaste,
			PixQRCodeBase64: item.PixQRCodeBase64,
		},
		PaidAt:    item.PaidAt,
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
	}
}

func toOrderResponses(items []domain.Order) []OrderResponse {
	responses := make([]OrderResponse, 0, len(items))
	for index := range items {
		responses = append(responses, toOrderResponse(&items[index]))
	}
	return responses
}
