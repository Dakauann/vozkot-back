package event

import (
	"strconv"
	"time"

	domain "vozkot/domain/event"
	mediadomain "vozkot/domain/media"
	"vozkot/domain/pricing"
	ticketdomain "vozkot/domain/ticket"
)

// The wire format lives here, not in the domain.
//
// A domain entity carrying json tags is a domain entity whose field names are
// an API contract: rename one and a client breaks. Mapping costs a function and
// buys the freedom to change either side alone.

type LocationRequest struct {
	Venue        string `json:"venue" example:"Arena Castelão"`
	Address      string `json:"address" example:"Av. Alberto Craveiro, 2901"`
	Neighborhood string `json:"neighborhood" example:"Castelão"`
	City         string `json:"city" example:"Fortaleza"`
	UF           string `json:"uf" example:"CE"`
	PostalCode   string `json:"postalCode" example:"60861-630"`
	// Latitude and Longitude are optional and travel together. Supplying them
	// means "a person placed this pin", and the server will not overwrite them
	// with whatever a geocoder thinks.
	Latitude  *float64 `json:"latitude,omitempty" example:"-3.807"`
	Longitude *float64 `json:"longitude,omitempty" example:"-38.522"`
}

type CreateRequest struct {
	Name        string          `json:"name" example:"Festival Aurora"`
	Description string          `json:"description"`
	Category    string          `json:"category" example:"festas_shows"`
	Location    LocationRequest `json:"location"`
	StartsAt    time.Time       `json:"startsAt" example:"2026-11-15T22:00:00-03:00"`
	EndsAt      *time.Time      `json:"endsAt,omitempty"`
	Status      string          `json:"status" enums:"draft,published,cancelled" example:"draft"`
	// SalesMode is how this event sells: `counted` by the number, the way a
	// party does, or `seated` by the chair, with a row and a seat number. It is
	// a declaration and not a fact about inventory — the seats themselves
	// answer whether a night has any — but nothing else can be derived from an
	// event that has no tiers yet, and the interface has to know which question
	// to ask next. Empty means counted.
	SalesMode string `json:"salesMode" enums:"counted,seated" example:"counted"`
}

type UpdateRequest = CreateRequest

// PinRequest is an operator dragging the map pin to where the venue really is.
type PinRequest struct {
	Latitude  float64 `json:"latitude" example:"-3.807"`
	Longitude float64 `json:"longitude" example:"-38.522"`
}

type LocationResponse struct {
	Venue        string   `json:"venue"`
	Address      string   `json:"address"`
	Neighborhood string   `json:"neighborhood"`
	City         string   `json:"city"`
	UF           string   `json:"uf"`
	PostalCode   string   `json:"postalCode"`
	Latitude     *float64 `json:"latitude,omitempty"`
	Longitude    *float64 `json:"longitude,omitempty"`
	// MapsURL and WazeURL are built server-side so every client opens the same
	// place, and so a client never has to know the URL shape of a map provider.
	// Empty when the event has no coordinates.
	MapsURL string `json:"mapsUrl,omitempty"`
	WazeURL string `json:"wazeUrl,omitempty"`
}

type MediaResponse struct {
	ID          string `json:"id"`
	Kind        string `json:"kind" enums:"image,video"`
	URL         string `json:"url"`
	ContentType string `json:"contentType"`
	SizeBytes   int64  `json:"sizeBytes"`
	Position    int    `json:"position"`
	// Width, Height and BlurDataURL are what a client needs to reserve the
	// right box and paint something before the bytes arrive. Zero and empty
	// when the asset predates the pipeline that generates them.
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	BlurDataURL string `json:"blurDataUrl,omitempty"`
}

type EventResponse struct {
	ID          string           `json:"id" example:"evt_a1b2c3d4"`
	Slug        string           `json:"slug" example:"festival-aurora"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Category    string           `json:"category"`
	Location    LocationResponse `json:"location"`
	StartsAt    time.Time        `json:"startsAt"`
	EndsAt      *time.Time       `json:"endsAt,omitempty"`
	Status      string           `json:"status"`
	SalesMode   string           `json:"salesMode" enums:"counted,seated"`
	Media       []MediaResponse  `json:"media"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`
}

// ListingResponse is a card: the event plus the two numbers a card shows that
// do not live on the event.
type ListingResponse struct {
	EventResponse
	// FromPriceCents is the cheapest tier still on sale. Null renders as
	// "Esgotado" rather than as "free".
	FromPriceCents   *int64 `json:"fromPriceCents"`
	AvailableTickets int    `json:"availableTickets"`
}

type EventEnvelope struct {
	Data EventResponse `json:"data"`
}

type ListEnvelope struct {
	Data   []ListingResponse `json:"data"`
	Total  int64             `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

// FiltersEnvelope is what the listing page renders its own controls from.
type FiltersEnvelope struct {
	Data FiltersResponse `json:"data"`
}

type FiltersResponse struct {
	Categories []CategoryResponse `json:"categories"`
	Cities     []CityResponse     `json:"cities"`
}

type CategoryResponse struct {
	Value string `json:"value" example:"festas_shows"`
	// Count lets the UI grey out an empty category instead of hiding it. A
	// filter row whose options come and go is one a buyer cannot learn.
	Count int64 `json:"count"`
}

type CityResponse struct {
	City  string `json:"city"`
	UF    string `json:"uf"`
	Count int64  `json:"count"`
}

// TierResponse is one price an event sells at.
type TierResponse struct {
	ID          string `json:"id"`
	EventID     string `json:"eventId"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Centavos. No float ever touches a price.
	//
	// PriceCents is the FACE value the organiser set. FeeCents is the service
	// charge added on top of one ticket, and TotalCents is what the buyer will
	// actually pay for it — the number that has to appear on the event page,
	// because a total that first shows up at the last step of checkout is the
	// single largest cause of an abandoned cart.
	PriceCents int64  `json:"priceCents"`
	FeeCents   int64  `json:"feeCents"`
	TotalCents int64  `json:"totalCents"`
	Currency   string `json:"currency"`
	Quantity   int    `json:"quantity"`
	Sold       int    `json:"sold"`
	Available  int    `json:"available"`
	Status     string `json:"status"`
}

type TierListEnvelope struct {
	Data []TierResponse `json:"data"`
}

// toTierResponses prices each tier the way checkout will.
//
// The fee is applied HERE, from the same pricing.Fee the checkout service
// carries, rather than being recomputed from a rate the client is told: the
// number quoted on the event page and the number charged on the order have to
// come from one place, and this is the only way they can.
func toTierResponses(items []ticketdomain.Ticket, fee pricing.Fee) []TierResponse {
	responses := make([]TierResponse, 0, len(items))
	for index := range items {
		item := items[index]
		priced := fee.Quote(item.PriceCents, 1)
		responses = append(responses, TierResponse{
			ID:          item.ID,
			EventID:     item.EventID,
			Title:       item.Title,
			Description: item.Description,
			PriceCents:  item.PriceCents,
			FeeCents:    priced.UnitFeeCents,
			TotalCents:  priced.TotalCents,
			Currency:    item.Currency,
			Quantity:    item.Quantity,
			Sold:        item.Sold,
			Available:   item.Available(),
			Status:      string(item.Status),
		})
	}
	return responses
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func toLocationRequest(location LocationRequest) domain.Location {
	return domain.Location{
		Venue:        location.Venue,
		Address:      location.Address,
		Neighborhood: location.Neighborhood,
		City:         location.City,
		UF:           location.UF,
		PostalCode:   location.PostalCode,
		Latitude:     location.Latitude,
		Longitude:    location.Longitude,
	}
}

func toEventResponse(item *domain.Event) EventResponse {
	return EventResponse{
		ID:          item.ID,
		Slug:        item.Slug,
		Name:        item.Name,
		Description: item.Description,
		Category:    string(item.Category),
		Location:    toLocationResponse(item.Location),
		StartsAt:    item.StartsAt,
		EndsAt:      item.EndsAt,
		Status:      string(item.Status),
		SalesMode:   string(item.SalesMode),
		Media:       toMediaResponses(item.Media),
		CreatedAt:   item.CreatedAt,
		UpdatedAt:   item.UpdatedAt,
	}
}

func toLocationResponse(location domain.Location) LocationResponse {
	response := LocationResponse{
		Venue:        location.Venue,
		Address:      location.Address,
		Neighborhood: location.Neighborhood,
		City:         location.City,
		UF:           location.UF,
		PostalCode:   location.PostalCode,
		Latitude:     location.Latitude,
		Longitude:    location.Longitude,
	}
	if location.HasCoordinates() {
		point := formatCoordinates(*location.Latitude, *location.Longitude)
		// The universal cross-platform forms, which open the installed app on a
		// phone and the website on a desktop.
		response.MapsURL = "https://www.google.com/maps/search/?api=1&query=" + point
		response.WazeURL = "https://waze.com/ul?ll=" + point + "&navigate=yes"
	}
	return response
}

// formatCoordinates renders a point for a map URL.
//
// 'f' with six decimals rather than %v: the default float formatting would emit
// scientific notation for a small longitude, and no map provider parses that.
// Six decimals is about ten centimetres, which is far finer than any venue pin
// needs and short enough to keep the URL readable.
func formatCoordinates(latitude, longitude float64) string {
	return strconv.FormatFloat(latitude, 'f', 6, 64) + "," + strconv.FormatFloat(longitude, 'f', 6, 64)
}

func toMediaResponses(items []mediadomain.Media) []MediaResponse {
	responses := make([]MediaResponse, 0, len(items))
	for index := range items {
		responses = append(responses, MediaResponse{
			ID:          items[index].ID,
			Kind:        string(items[index].Kind),
			URL:         items[index].URL,
			ContentType: items[index].ContentType,
			SizeBytes:   items[index].SizeBytes,
			Position:    items[index].Position,
			Width:       items[index].Width,
			Height:      items[index].Height,
			BlurDataURL: items[index].BlurDataURL,
		})
	}
	return responses
}

func toListingResponses(items []domain.Listing) []ListingResponse {
	responses := make([]ListingResponse, 0, len(items))
	for index := range items {
		responses = append(responses, ListingResponse{
			EventResponse:    toEventResponse(&items[index].Event),
			FromPriceCents:   items[index].FromPriceCents,
			AvailableTickets: items[index].AvailableTickets,
		})
	}
	return responses
}
