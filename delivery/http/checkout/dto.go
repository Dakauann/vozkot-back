package checkout

import (
	"time"

	eventdomain "vozkot/domain/event"
	domain "vozkot/domain/order"
)

// The wire format for purchases. As with tickets, the domain entity carries no
// json tags and this file is the only place the API's field names live.

// CheckoutRequest opens a basket: one event's tiers, and how many of each.
type CheckoutRequest struct {
	// Items is the basket. Every tier must belong to the same event.
	Items []CheckoutItem `json:"items"`
	// TicketID and Quantity are the single-tier shorthand, kept because an
	// operator selling one tier at the door, and the load harness, have no
	// use for a list of one. Ignored when Items is present.
	TicketID string `json:"ticketId,omitempty" example:"tkt_a1b2c3d4"`
	Quantity int    `json:"quantity,omitempty" example:"2"`
	// Method is optional and defaults to PIX, the only instrument this
	// integration can issue from a server.
	Method string `json:"method,omitempty" enums:"pix" example:"pix"`
	// Buyer is optional here. The browser reserves first and supplies the buyer
	// at /confirm, so that the tickets come off the shelf while the form is
	// being filled in rather than after it. Supplying it here does both at
	// once.
	Buyer Buyer `json:"buyer"`
	// Confirm asks for the charge immediately, instead of waiting for the
	// confirm step. It requires Buyer to be complete.
	Confirm bool `json:"confirm,omitempty" example:"false"`
}

// CheckoutItem is one tier and how many of it.
type CheckoutItem struct {
	TicketID string `json:"ticketId" example:"tkt_a1b2c3d4"`
	Quantity int    `json:"quantity" example:"2"`
}

// Items resolves the basket, accepting either shape.
func (r CheckoutRequest) Lines() []domain.DraftItem {
	if len(r.Items) > 0 {
		lines := make([]domain.DraftItem, 0, len(r.Items))
		for _, line := range r.Items {
			lines = append(lines, domain.DraftItem{TicketID: line.TicketID, Quantity: line.Quantity})
		}
		return lines
	}
	if r.TicketID == "" {
		return nil
	}
	return []domain.DraftItem{{TicketID: r.TicketID, Quantity: r.Quantity}}
}

// ConfirmRequest is the details step: who is buying, and how.
type ConfirmRequest struct {
	Buyer  Buyer  `json:"buyer"`
	Method string `json:"method,omitempty" enums:"pix" example:"pix"`
}

type Buyer struct {
	Name string `json:"name" example:"Maria Souza"`
	// Email receives the ingresso, and identifies the payer to the provider.
	Email string `json:"email" example:"maria@exemplo.com.br"`
	// Document is the CPF or CNPJ. PIX in Brazil cannot be issued without one.
	Document string `json:"document" example:"12345678909"`
}

type CancelRequest struct {
	Reason string `json:"reason,omitempty" example:"Desisti da compra"`
}

// OrderItemResponse is one tier's line on an order.
//
// The title is the one recorded at purchase, not the tier's current name: a
// receipt has to keep saying what was bought after the organiser renames it.
type OrderItemResponse struct {
	TicketID       string `json:"ticketId" example:"tkt_a1b2c3d4"`
	TicketTitle    string `json:"ticketTitle" example:"Pista"`
	Quantity       int    `json:"quantity" example:"2"`
	UnitPriceCents int64  `json:"unitPriceCents" example:"24000"`
	TotalCents     int64  `json:"totalCents" example:"48000"`
}

// OrderEventResponse is the night an order is for.
//
// Present on listings, where an order that named only ids would be unreadable,
// and absent when the event has been deleted, which costs a row its title and
// nothing else.
type OrderEventResponse struct {
	ID       string    `json:"id" example:"evt_7c1a"`
	Slug     string    `json:"slug" example:"festival-de-verao-2026"`
	Name     string    `json:"name" example:"Festival de Verão 2026"`
	StartsAt time.Time `json:"startsAt"`
	Venue    string    `json:"venue" example:"Arena Fonte Nova"`
	City     string    `json:"city" example:"Salvador"`
	UF       string    `json:"uf" example:"BA"`
	CoverURL string    `json:"coverUrl,omitempty"`
}

type OrderResponse struct {
	ID      string `json:"id" example:"ord_9f2c1d8a"`
	EventID string `json:"eventId" example:"evt_7c1a"`
	// Items is every tier on the order, never empty.
	Items []OrderItemResponse `json:"items"`
	// Quantity is the total across every line, so a client that only wants the
	// headline number does not have to add them up.
	Quantity int `json:"quantity" example:"3"`

	BuyerName  string `json:"buyerName" example:"Maria Souza"`
	BuyerEmail string `json:"buyerEmail" example:"maria@exemplo.com.br"`

	TotalCents int64  `json:"totalCents" example:"48000"`
	Currency   string `json:"currency" example:"BRL"`

	Status string `json:"status" enums:"pending_payment,paid,expired,cancelled,failed,refunded,refund_required" example:"pending_payment"`
	// HoldExpiresAt is when the reserved tickets go back on sale. It is the
	// clock a checkout screen counts down.
	HoldExpiresAt time.Time `json:"holdExpiresAt"`
	// Confirmed reports whether the buyer has supplied their details. An
	// unconfirmed order is a basket on the short cart hold and has no charge.
	Confirmed bool `json:"confirmed" example:"false"`

	Payment PaymentResponse `json:"payment"`
	// Event is the night, resolved for listings. Omitted when unavailable.
	Event *OrderEventResponse `json:"event,omitempty"`

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
	// Provider is empty until a charge exists: an order that has not been
	// charged has not been through one.
	Provider string `json:"provider" example:"asaas"`
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
	lines := make([]OrderItemResponse, 0, len(item.Items))
	for _, line := range item.Items {
		lines = append(lines, OrderItemResponse{
			TicketID:       line.TicketID,
			TicketTitle:    line.TicketTitle,
			Quantity:       line.Quantity,
			UnitPriceCents: line.UnitPriceCents,
			TotalCents:     line.TotalCents,
		})
	}
	return OrderResponse{
		ID:            item.ID,
		EventID:       item.EventID,
		Items:         lines,
		Quantity:      item.TotalQuantity(),
		BuyerName:     item.BuyerName,
		BuyerEmail:    item.BuyerEmail,
		TotalCents:    item.TotalCents,
		Currency:      item.Currency,
		Status:        string(item.Status),
		HoldExpiresAt: item.HoldExpiresAt,
		Confirmed:     item.Confirmed,
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

// attachEvents fills in the night behind each order, from events already read.
//
// Separate from toOrderResponse because resolving them is a query, and a query
// belongs to the handler that can batch it across a page rather than to a
// mapping function called once per row.
func attachEvents(responses []OrderResponse, events map[string]*eventdomain.Event) {
	for index := range responses {
		happening, found := events[responses[index].EventID]
		if !found || happening == nil {
			continue
		}
		summary := OrderEventResponse{
			ID:       happening.ID,
			Slug:     happening.Slug,
			Name:     happening.Name,
			StartsAt: happening.StartsAt,
			Venue:    happening.Location.Venue,
			City:     happening.Location.City,
			UF:       happening.Location.UF,
		}
		if len(happening.Media) > 0 {
			summary.CoverURL = happening.Media[0].URL
		}
		responses[index].Event = &summary
	}
}
