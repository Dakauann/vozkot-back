package report

import (
	"errors"
	"net/http"
	"time"

	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	domain "vozkot/domain/report"
)

// ErrorResponse mirrors httpx.ErrorResponse for the generated documentation.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// SliceResponse is one row of a breakdown.
//
// Keys are stable identifiers, `female`, `SP`, `24_28`, `unknown`, and never
// translated strings. The client owns the wording, which is what lets the same
// payload render in four locales, and `unknown` is a real row rather than an
// omission so the percentages add up.
type SliceResponse struct {
	Key   string `json:"key" example:"SP"`
	Label string `json:"label,omitempty" example:"Pista"`
	// Orders is how many purchases; Tickets is how many admissions. One order
	// for eight tickets is one buyer and eight seats, and a table showing only
	// one of them is wrong for half the decisions made from it.
	Orders  int `json:"orders" example:"412"`
	Tickets int `json:"tickets" example:"937"`
	// NetCents is the organiser's earnings for this slice. It is the only
	// money field here; see the money note in domain/report.
	NetCents int64 `json:"netCents" example:"9370000"`
}

// DayResponse is one day of the sales curve.
type DayResponse struct {
	// Day is YYYY-MM-DD in UTC, the day the money LANDED.
	Day      string `json:"day" example:"2026-07-26"`
	Orders   int    `json:"orders" example:"612"`
	Tickets  int    `json:"tickets" example:"1592"`
	NetCents int64  `json:"netCents" example:"15920000"`
}

// TotalsResponse is the headline.
type TotalsResponse struct {
	Orders  int `json:"orders" example:"12430"`
	Tickets int `json:"tickets" example:"35735"`
	// Buyers is DISTINCT accounts: how many PEOPLE, not how many purchases.
	Buyers int `json:"buyers" example:"11902"`
	// NetCents is what the organiser earned: the sum of the face values they
	// priced. RefundedCents is at face value too.
	//
	// Neither the gross the buyer paid nor our commission is sent, and not
	// because they are filtered out here: the query never selects them. See
	// the money note in domain/report.
	NetCents       int64 `json:"netCents" example:"357350000"`
	RefundedOrders int   `json:"refundedOrders" example:"87"`
	RefundedCents  int64 `json:"refundedCents" example:"2750000"`
	// AverageOrderCents and AverageTicketCents are the ticket médio, asked the
	// two ways an organiser means it: what one buyer spends in a go, and what
	// one admission is worth. They differ whenever anybody buys for a group,
	// and only the second is evidence for repricing a tier.
	//
	// Sent rather than left to the client to divide, so the API, the CSV and
	// every screen round the same way.
	AverageOrderCents  int64 `json:"averageOrderCents" example:"28750"`
	AverageTicketCents int64 `json:"averageTicketCents" example:"10000"`
}

// SalesResponse is the whole dashboard for one event.
type SalesResponse struct {
	EventID  string          `json:"eventId" example:"evt_88"`
	Totals   TotalsResponse  `json:"totals"`
	ByGender []SliceResponse `json:"byGender"`
	ByAge    []SliceResponse `json:"byAge"`
	ByUF     []SliceResponse `json:"byUf"`
	ByCity   []SliceResponse `json:"byCity"`
	ByTier   []SliceResponse `json:"byTier"`
	ByDay    []DayResponse   `json:"byDay"`
}

// AttendeeResponse is one person who bought, as the organiser may see them.
//
// The document is MASKED and the date of birth and phone are absent entirely.
// An organiser needs to match somebody to the ID they present at the door; they
// do not need to be able to be that person at a bank.
type AttendeeResponse struct {
	OrderID     string    `json:"orderId" example:"ord_4a1b7c"`
	PurchasedAt time.Time `json:"purchasedAt"`
	Status      string    `json:"status" example:"paid"`

	Name         string `json:"name" example:"Maria Souza"`
	Email        string `json:"email" example:"maria@exemplo.com.br"`
	DocumentMask string `json:"documentMask,omitempty" example:"529***25"`

	Gender string `json:"gender,omitempty" example:"female"`
	// AgeYears is the buyer's age AT PURCHASE, 0 when not informed.
	AgeYears int    `json:"ageYears,omitempty" example:"31"`
	City     string `json:"city,omitempty" example:"Natal"`
	UF       string `json:"uf,omitempty" example:"RN"`

	TicketID       string `json:"ticketId" example:"tkt_9f"`
	TicketTitle    string `json:"ticketTitle" example:"Pista"`
	Quantity       int    `json:"quantity" example:"2"`
	UnitPriceCents int64  `json:"unitPriceCents" example:"24000"`
	NetCents       int64  `json:"netCents" example:"48000"`
	// AdmittedCount is how many of this line's Quantity tickets have entered.
	AdmittedCount int `json:"admittedCount" example:"2"`
}

// AttendeeListEnvelope is a page of attendees.
type AttendeeListEnvelope struct {
	Data   []AttendeeResponse `json:"data"`
	Total  int64              `json:"total"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
}

// SalesEnvelope wraps the dashboard.
type SalesEnvelope struct {
	Data SalesResponse `json:"data"`
}

func toSalesResponse(sales domain.Sales) SalesResponse {
	return SalesResponse{
		EventID: sales.EventID,
		Totals: TotalsResponse{
			Orders:         sales.Totals.Orders,
			Tickets:        sales.Totals.Tickets,
			Buyers:         sales.Totals.Buyers,
			NetCents:       sales.Totals.NetCents,
			RefundedOrders: sales.Totals.RefundedOrders,
			RefundedCents:  sales.Totals.RefundedCents,
			// Derived in the domain so the ratio has one definition.
			AverageOrderCents:  sales.Totals.AverageOrderCents(),
			AverageTicketCents: sales.Totals.AverageTicketCents(),
		},
		ByGender: toSlices(sales.ByGender),
		ByAge:    toSlices(sales.ByAge),
		ByUF:     toSlices(sales.ByUF),
		ByCity:   toSlices(sales.ByCity),
		ByTier:   toSlices(sales.ByTier),
		ByDay:    toDays(sales.ByDay),
	}
}

func toSlices(slices []domain.Slice) []SliceResponse {
	// A non-nil empty slice, so an event with no sales serialises as [] and the
	// client can map over it without a null check per breakdown.
	responses := make([]SliceResponse, 0, len(slices))
	for _, slice := range slices {
		responses = append(responses, SliceResponse{
			Key:      slice.Key,
			Label:    slice.Label,
			Orders:   slice.Orders,
			Tickets:  slice.Tickets,
			NetCents: slice.NetCents,
		})
	}
	return responses
}

func toDays(days []domain.DaySlice) []DayResponse {
	responses := make([]DayResponse, 0, len(days))
	for _, day := range days {
		responses = append(responses, DayResponse{
			Day:      day.Day.Format("2006-01-02"),
			Orders:   day.Orders,
			Tickets:  day.Tickets,
			NetCents: day.NetCents,
		})
	}
	return responses
}

func toAttendees(items []domain.Attendee) []AttendeeResponse {
	responses := make([]AttendeeResponse, 0, len(items))
	for _, item := range items {
		responses = append(responses, AttendeeResponse{
			OrderID:        item.OrderID,
			PurchasedAt:    item.PurchasedAt,
			Status:         item.Status,
			Name:           item.Name,
			Email:          item.Email,
			DocumentMask:   item.DocumentMask,
			Gender:         item.Gender,
			AgeYears:       item.AgeYears,
			City:           item.City,
			UF:             item.UF,
			TicketID:       item.TicketID,
			TicketTitle:    item.TicketTitle,
			Quantity:       item.Quantity,
			UnitPriceCents: item.UnitPriceCents,
			NetCents:       item.NetCents,
			AdmittedCount:  item.AdmittedCount,
		})
	}
	return responses
}

// StatusFor maps every error these endpoints can produce onto HTTP.
func StatusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, authdomain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, authdomain.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, eventdomain.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
