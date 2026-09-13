package ticket

import (
	"time"

	mediadomain "vozkot/domain/media"
	domain "vozkot/domain/ticket"
)

// The wire format lives here, not in the domain.
//
// A domain entity carrying json tags is a domain entity whose field names are
// an API contract: rename one and a client breaks. Mapping costs a function and
// buys the freedom to change either side alone.

type CreateRequest struct {
	// EventID is the happening this tier sells admission to. The event owns the
	// name, venue, city, date, category and map pin.
	EventID     string `json:"eventId" example:"evt_a1b2c3d4"`
	Title       string `json:"title" example:"Pista Premium"`
	Description string `json:"description" example:"Acesso à área premium com bar exclusivo."`
	PriceCents  int64  `json:"priceCents" example:"24000"`
	Quantity    int    `json:"quantity" example:"500"`
	Status      string `json:"status" enums:"draft,on_sale,sold_out,cancelled" example:"draft"`
}

type UpdateRequest struct {
	Title       string `json:"title" example:"Pista Premium"`
	Description string `json:"description" example:"Acesso à área premium com bar exclusivo."`
	PriceCents  int64  `json:"priceCents" example:"24000"`
	Quantity    int    `json:"quantity" example:"500"`
	Status      string `json:"status" enums:"draft,on_sale,sold_out,cancelled" example:"on_sale"`
}

type ChangeStatusRequest struct {
	Status string `json:"status" enums:"draft,on_sale,sold_out,cancelled" example:"on_sale"`
}

type MediaResponse struct {
	ID          string    `json:"id" example:"med_9f2c1d8a"`
	Kind        string    `json:"kind" enums:"image,video" example:"image"`
	URL         string    `json:"url" example:"https://cdn.exemplo.com/tickets/tkt_a1b2/med_9f2c1d8a.jpg"`
	ContentType string    `json:"contentType" example:"image/jpeg"`
	SizeBytes   int64     `json:"sizeBytes" example:"284133"`
	Position    int       `json:"position" example:"0"`
	CreatedAt   time.Time `json:"createdAt"`
}

type TicketResponse struct {
	ID          string `json:"id" example:"tkt_a1b2c3d4"`
	EventID     string `json:"eventId" example:"evt_a1b2c3d4"`
	Title       string `json:"title" example:"Pista Premium"`
	Description string `json:"description"`
	PriceCents  int64  `json:"priceCents" example:"24000"`
	Currency    string `json:"currency" example:"BRL"`
	Quantity    int    `json:"quantity" example:"500"`
	Sold        int    `json:"sold" example:"128"`
	// Reserved is stock held by orders waiting to be paid. An operator looking
	// at a tier that is nearly gone needs to know whether it went to sales or
	// to holds, because those two have completely different answers.
	Reserved  int             `json:"reserved" example:"12"`
	Available int             `json:"available" example:"360"`
	Status    string          `json:"status" enums:"draft,on_sale,sold_out,cancelled" example:"on_sale"`
	Media     []MediaResponse `json:"media"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

type TicketEnvelope struct {
	Data TicketResponse `json:"data"`
}

type TicketListEnvelope struct {
	Data   []TicketResponse `json:"data"`
	Total  int64            `json:"total" example:"42"`
	Limit  int              `json:"limit" example:"20"`
	Offset int              `json:"offset" example:"0"`
}

type MediaEnvelope struct {
	Data MediaResponse `json:"data"`
}

type ErrorResponse struct {
	Error string `json:"error" example:"ticket not found"`
}

func toTicketResponse(item *domain.Ticket) TicketResponse {
	return TicketResponse{
		ID:          item.ID,
		EventID:     item.EventID,
		Title:       item.Title,
		Description: item.Description,
		PriceCents:  item.PriceCents,
		Currency:    item.Currency,
		Quantity:    item.Quantity,
		Sold:        item.Sold,
		Reserved:    item.Reserved,
		// Derived on the way out: a client that computes it itself will one day
		// compute it differently from the entity, and then the two disagree.
		Available: item.Available(),
		Status:    string(item.Status),
		Media:     toMediaResponses(item.Media),
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
	}
}

func toTicketResponses(items []domain.Ticket) []TicketResponse {
	responses := make([]TicketResponse, 0, len(items))
	for index := range items {
		responses = append(responses, toTicketResponse(&items[index]))
	}
	return responses
}

func toMediaResponse(item *mediadomain.Media) MediaResponse {
	return MediaResponse{
		ID:          item.ID,
		Kind:        string(item.Kind),
		URL:         item.URL,
		ContentType: item.ContentType,
		SizeBytes:   item.SizeBytes,
		Position:    item.Position,
		CreatedAt:   item.CreatedAt,
	}
}

func toMediaResponses(items []mediadomain.Media) []MediaResponse {
	// Never nil: an absent gallery serializes as [] so clients can map over it
	// without a null check.
	responses := make([]MediaResponse, 0, len(items))
	for index := range items {
		responses = append(responses, toMediaResponse(&items[index]))
	}
	return responses
}
