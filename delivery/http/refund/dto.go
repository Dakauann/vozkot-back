package refund

import (
	"errors"
	"net/http"
	"time"

	authdomain "vozkot/domain/auth"
	orderdomain "vozkot/domain/order"
	domain "vozkot/domain/refund"
	usecase "vozkot/usecases/refund"
)

// ErrorResponse mirrors httpx.ErrorResponse for the generated documentation.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// EligibilityResponse is what the order page reads before drawing a button.
//
// It answers both halves: whether a refund is allowed, and if not, WHY. A bare
// boolean would leave the screen with a disabled button and nothing to say,
// which is the single most common way a self-service flow becomes a support
// queue.
type EligibilityResponse struct {
	Allowed bool `json:"allowed" example:"true"`
	// Until is when the right to cancel expires, so the page can render a real
	// date instead of "soon". Absent for grounds that have no deadline.
	Until *time.Time `json:"until,omitempty"`
	// Refusal is the stable name of the reason, for a client to branch on:
	// window_closed, too_close_to_event, not_paid, already_refunded,
	// event_passed, request_open.
	Refusal string `json:"refusal,omitempty" example:"too_close_to_event"`
	// AmountCents is what would come back to the BUYER and FeeCents how much of
	// it is our commission. Both are pointers and both are OMITTED for an
	// organiser: see SeesPlatformShare in usecases/refund.
	//
	// Omitted rather than zeroed. A zero here would render as "R$ 0,00 comes
	// back", which is worse than saying nothing.
	AmountCents *int64 `json:"amountCents,omitempty" example:"11000"`
	FeeCents    *int64 `json:"feeCents,omitempty" example:"1000"`
	// OrganiserCents is the part of the refund that comes out of the
	// organiser's revenue. Always sent, to everyone.
	OrganiserCents int64 `json:"organiserCents" example:"10000"`
	// RefundsFees says the fee is included. Rendered beside the amount because
	// "R$ 110,00, taxa incluída" is the sentence that prevents the dispute.
	RefundsFees bool `json:"refundsFees" example:"true"`
	// Request is the one already in flight, when there is one.
	Request *Response `json:"request,omitempty"`
}

// RequestBody is a buyer or an organiser asking for money back.
type RequestBody struct {
	// Reason is the grounds. A buyer may send buyer_withdrawal or
	// event_cancelled and nothing else; the other two are refused for them,
	// because they are the grounds with no window.
	Reason string `json:"reason" enums:"buyer_withdrawal,event_cancelled,organiser_goodwill" example:"buyer_withdrawal"`
	Note   string `json:"note,omitempty" example:"Não vou conseguir ir"`
}

// DecisionBody is an organiser or an operator answering a pending request.
type DecisionBody struct {
	Note string `json:"note,omitempty" example:"Reembolso autorizado"`
}

// Response is one refund request.
type Response struct {
	ID      string `json:"id" example:"rfr_9f2c1d8a"`
	OrderID string `json:"orderId" example:"ord_4a1b7c"`
	EventID string `json:"eventId,omitempty" example:"evt_88"`
	Reason  string `json:"reason" example:"buyer_withdrawal"`
	// Status is pending, approved or rejected: where the DECISION stands.
	//
	// Whether the money has actually arrived is a different question and is
	// answered by `orderStatus` below, which is the order's own. Two fields
	// rather than one combined status, because only one of them is stored and
	// the two therefore cannot disagree; a single "completed" written here
	// would be a second writer for a fact the settlement path already owns.
	Status string `json:"status" example:"approved"`
	// AmountCents is what goes back to the BUYER and FeeCents how much of it is
	// our commission. Omitted for an organiser; see SeesPlatformShare.
	AmountCents *int64 `json:"amountCents,omitempty" example:"11000"`
	FeeCents    *int64 `json:"feeCents,omitempty" example:"1000"`
	// OrganiserCents is what comes out of the organiser's revenue. Always sent.
	OrganiserCents int64 `json:"organiserCents" example:"10000"`
	// AutoApproved is true when the POLICY decided rather than a person, which
	// is what the screen shows instead of naming a reviewer.
	AutoApproved bool       `json:"autoApproved" example:"true"`
	RequestedBy  string     `json:"requestedBy,omitempty" example:"usr_31"`
	Note         string     `json:"note,omitempty"`
	DecidedBy    string     `json:"decidedBy,omitempty"`
	DecisionNote string     `json:"decisionNote,omitempty"`
	DecidedAt    *time.Time `json:"decidedAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// Envelope wraps one request, matching the shape every other endpoint uses.
type Envelope struct {
	Data Response `json:"data"`
}

// ListEnvelope is a page of requests.
type ListEnvelope struct {
	Data   []Response `json:"data"`
	Total  int64      `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}

// toResponse maps one request, hiding our side of the money unless the caller
// is entitled to it.
//
// showPlatformShare is decided by usecases/refund.SeesPlatformShare and passed
// in rather than worked out here: the transport must not be the place that
// knows who we show our commission to.
func toResponse(item *domain.Request, showPlatformShare bool) Response {
	response := Response{
		ID:      item.ID,
		OrderID: item.OrderID,
		EventID: item.EventID,
		Reason:  string(item.Reason),
		Status:  string(item.Status),
		// The organiser's share is the face value: the gross minus ours.
		OrganiserCents: item.AmountCents - item.FeeCents,
		AutoApproved:   item.AutoApproved(),
		RequestedBy:    item.RequestedBy,
		Note:           item.Note,
		DecidedBy:      item.DecidedBy,
		DecisionNote:   item.DecisionNote,
		DecidedAt:      item.DecidedAt,
		CreatedAt:      item.CreatedAt,
		UpdatedAt:      item.UpdatedAt,
	}
	if showPlatformShare {
		amount, fee := item.AmountCents, item.FeeCents
		response.AmountCents = &amount
		response.FeeCents = &fee
	}
	return response
}

// toResponses maps a page, deciding visibility PER ROW.
//
// Per row and not once for the page, because one listing can mix both: a buyer
// asking for their own history sees their money, and the same call by an
// organiser scoped to their event sees none of ours. A single flag for the
// whole page would be right for one of them and wrong for the other.
func toResponses(items []domain.Request, actor authdomain.Actor) []Response {
	responses := make([]Response, 0, len(items))
	for index := range items {
		item := &items[index]
		responses = append(responses, toResponse(
			item,
			usecase.SeesPlatformShare(actor, item.BuyerID, item.RequestedBy),
		))
	}
	return responses
}

func toEligibility(eligibility usecase.Eligibility, open *domain.Request) EligibilityResponse {
	response := EligibilityResponse{
		Allowed:        eligibility.Decision.Allowed,
		Refusal:        string(eligibility.Decision.Refusal),
		OrganiserCents: eligibility.OrganiserCents,
		RefundsFees:    eligibility.RefundsFees,
	}
	// The use case already decided whether this caller may see our side; this
	// only honours it.
	if eligibility.ShowsPlatformShare {
		amount, fee := eligibility.AmountCents, eligibility.FeeCents
		response.AmountCents = &amount
		response.FeeCents = &fee
	}
	if !eligibility.Decision.Until.IsZero() {
		until := eligibility.Decision.Until
		response.Until = &until
	}
	if open != nil {
		// The nested request follows the same decision: an organiser reading an
		// eligibility answer must not get our numbers through the open request
		// hanging off it.
		request := toResponse(open, eligibility.ShowsPlatformShare)
		response.Request = &request
	}
	return response
}

// Machine-readable codes for the refusals a buyer can act on.
//
// The message beside them is a sentence for a person, in one language. A client
// that has to show "you had until the 12th" differently from "this show is in
// six hours" needs something stable, and this is it.
const (
	// CodeWindowClosed: the seven-day right of withdrawal has run out.
	CodeWindowClosed = "window_closed"
	// CodeTooCloseToEvent: inside the cutoff before the doors, or after them.
	CodeTooCloseToEvent = "too_close_to_event"
	// CodeRequestOpen: a request is already in flight on this order.
	CodeRequestOpen = "request_open"
	// CodeNotRefundable: the order never took money, or already gave it back.
	CodeNotRefundable = "not_refundable"
	// CodeReasonNotYours: grounds the caller has no standing to claim.
	CodeReasonNotYours = "reason_not_yours"
	// CodeAlreadyDecided: somebody else answered this request first.
	CodeAlreadyDecided = "already_decided"
)

// StatusFor maps every error these endpoints can produce onto HTTP.
func StatusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, authdomain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, usecase.ErrForbidden), errors.Is(err, domain.ErrReasonNotYours):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, orderdomain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrAlreadyOpen), errors.Is(err, domain.ErrNotDecidable):
		// 409: a conflict with state the caller can see and act on. There is
		// already a request, or somebody already answered it.
		return http.StatusConflict
	case errors.Is(err, domain.ErrWindowClosed),
		errors.Is(err, domain.ErrTooCloseToEvent),
		errors.Is(err, domain.ErrNotRefundable),
		errors.Is(err, domain.ErrInvalidReason),
		errors.Is(err, domain.ErrInvalidAmount):
		// 422: the request was well formed and the rules refuse it.
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// CodeFor names what went wrong, or "" when there is nothing useful to say.
func CodeFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, domain.ErrWindowClosed):
		return CodeWindowClosed
	case errors.Is(err, domain.ErrTooCloseToEvent):
		return CodeTooCloseToEvent
	case errors.Is(err, domain.ErrAlreadyOpen):
		return CodeRequestOpen
	case errors.Is(err, domain.ErrNotDecidable):
		return CodeAlreadyDecided
	case errors.Is(err, domain.ErrReasonNotYours):
		return CodeReasonNotYours
	case errors.Is(err, domain.ErrNotRefundable):
		return CodeNotRefundable
	default:
		return ""
	}
}
