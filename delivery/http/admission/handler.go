// Package admission is the transport for the door and for a holder's own
// tickets.
//
// Two audiences with opposite needs on the same data. The doorperson sends a
// code and gets back a verdict but never a code; the holder gets their codes
// and never a verdict about anybody else's. Both rules live in
// usecases/admission; this package translates a session into an actor, a body
// into a string, and an error into a status.
package admission

import (
	"errors"
	"net/http"
	"strings"

	"vozkot/delivery/http/httpx"
	domain "vozkot/domain/admission"
	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	orderdomain "vozkot/domain/order"
	seatingdomain "vozkot/domain/seating"
	usecase "vozkot/usecases/admission"
)

type Handler struct {
	admissions *usecase.Service
}

func NewHandler(admissions *usecase.Service) *Handler {
	return &Handler{admissions: admissions}
}

// Register mounts the door's routes under the event they belong to.
func (h *Handler) Register(router *http.ServeMux) {
	router.HandleFunc("POST /api/v1/events/{id}/scan", h.scan)
	router.HandleFunc("GET /api/v1/events/{id}/door", h.door)
}

// RegisterOrderRoutes mounts the holder's own tickets under their order.
func (h *Handler) RegisterOrderRoutes(router *http.ServeMux) {
	router.HandleFunc("GET /api/v1/orders/{id}/tickets", h.tickets)
	router.HandleFunc("GET /api/v1/orders/{id}/tickets/{admissionId}/qr.png", h.qr)
}

// ScanRequest is a code read from a camera or typed at a door.
type ScanRequest struct {
	// Code is whatever the scanner or the keyboard produced. It is normalised
	// and check-verified by the domain, so the grouped form, lower case and
	// the usual O-for-zero substitutions are all accepted.
	Code string `json:"code" example:"7QP5-2NFX-W78H"`
}

// ScanResponse is the verdict a door screen draws.
//
// It deliberately does NOT echo the code back. There is no use for it on the
// screen — the doorperson is holding the thing it came from — and an endpoint
// that returns codes is one XSS or one screenshot away from being a way to
// collect them.
type ScanResponse struct {
	// Outcome is the machine-readable verdict the client branches on:
	// admitted, already_admitted, void, wrong_event, not_paid, unknown,
	// malformed.
	Outcome string `json:"outcome" enums:"admitted,already_admitted,void,wrong_event,not_paid,unknown,malformed" example:"admitted"`
	// Admitted is the one boolean the screen needs to pick green or red.
	Admitted bool `json:"admitted" example:"true"`

	TicketTitle string `json:"ticketTitle,omitempty" example:"Pista"`
	// Seat is the reserved chair, absent for general admission.
	//
	// The most useful thing the door gains from reserved seating: somebody has
	// just been told they may come in, and the next thing they ask is where to
	// sit. Sent pre-formatted as well as in parts, so the screen and the email
	// cannot disagree about how a chair is named.
	Seat *SeatResponse `json:"seat,omitempty"`
	// Sequence and Total read as "2 de 3" on a group's tickets.
	Sequence       int    `json:"sequence,omitempty" example:"2"`
	OrderReference string `json:"orderReference,omitempty" example:"A1B2C3D4"`
	// AdmittedAt is when the code was FIRST used, which is what the holder is
	// told when it is refused as already used.
	AdmittedAt *string `json:"admittedAt,omitempty" example:"2026-09-16T21:14:03Z"`

	// Remaining and Admitted counters keep the door's screen live without a
	// second request.
	Remaining     int `json:"remaining" example:"318"`
	AdmittedCount int `json:"admittedCount" example:"682"`
}

// SeatResponse is a reserved chair, named the way an usher says it.
type SeatResponse struct {
	Section string `json:"section,omitempty" example:"Plateia A"`
	Row     string `json:"row,omitempty" example:"K"`
	Seat    string `json:"seat,omitempty" example:"12"`
	// Label is the whole thing, formatted once on the server.
	//
	// The door, the wallet, the receipt and the confirmation email all have to
	// say the same thing about a chair somebody is standing in front of, and
	// four copies of that format string is four chances to disagree.
	Label string `json:"label" example:"Plateia A · Fila K · Assento 12"`
}

// TicketResponse is one admission as its holder sees it.
type TicketResponse struct {
	ID          string        `json:"id" example:"adm_9f2c1d8a"`
	TicketTitle string        `json:"ticketTitle" example:"Pista"`
	Seat        *SeatResponse `json:"seat,omitempty"`
	Sequence    int           `json:"sequence" example:"2"`
	// Code is the printed, grouped form, and QRPayload is what a QR must
	// encode. Both are sent because the page draws both, and the client must
	// not have to know that one is the other with the hyphens removed.
	Code       string  `json:"code" example:"7QP5-2NFX-W78H"`
	QRPayload  string  `json:"qrPayload" example:"7QP52NFXW78H"`
	Status     string  `json:"status" enums:"issued,admitted,void" example:"issued"`
	AdmittedAt *string `json:"admittedAt,omitempty"`
}

type ScanEnvelope struct {
	Data ScanResponse `json:"data"`
}

type TicketListEnvelope struct {
	Data []TicketResponse `json:"data"`
}

type DoorResponse struct {
	Remaining     int `json:"remaining" example:"318"`
	AdmittedCount int `json:"admittedCount" example:"682"`
}

type DoorEnvelope struct {
	Data DoorResponse `json:"data"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// @Summary		Validar um ingresso na entrada
// @Description	Lê um código de ingresso, por QR ou digitado, e o consome se for válido. Cada código vale uma única entrada: a segunda leitura responde `already_admitted` e informa quando a primeira ocorreu. Um código de outro evento responde `wrong_event`, e um de pedido estornado responde `void`. Restrito ao organizador do evento e a administradores.
// @Tags			Entrada
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		request body ScanRequest true "Código lido"
// @Success		200 {object} ScanEnvelope
// @Failure		400 {object} ErrorResponse
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/events/{id}/scan [post]
func (h *Handler) scan(response http.ResponseWriter, request *http.Request) {
	var body ScanRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}

	result, err := h.admissions.Scan(
		request.Context(), actor(request), eventID(request), body.Code,
	)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, ScanEnvelope{Data: toScanResponse(result)})
}

// @Summary		Contadores da entrada
// @Description	Quantos ingressos do evento já entraram e quantos ainda faltam. É o número que a tela da portaria mostra. Restrito ao organizador do evento e a administradores.
// @Tags			Entrada
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Success		200 {object} DoorEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/events/{id}/door [get]
func (h *Handler) door(response http.ResponseWriter, request *http.Request) {
	remaining, admitted, err := h.admissions.Counters(request.Context(), actor(request), eventID(request))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, DoorEnvelope{
		Data: DoorResponse{Remaining: remaining, AdmittedCount: admitted},
	})
}

// @Summary		Meus ingressos
// @Description	Os ingressos de um pedido pago, com o código de cada um para apresentar na entrada. Restrito ao comprador do pedido e a administradores: o organizador do evento NÃO vê estes códigos, porque quem os tem pode entrar.
// @Tags			Entrada
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido"
// @Success		200 {object} TicketListEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/orders/{id}/tickets [get]
func (h *Handler) tickets(response http.ResponseWriter, request *http.Request) {
	issued, err := h.admissions.ForOrder(
		request.Context(), actor(request), strings.TrimSpace(request.PathValue("id")),
	)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}

	rows := make([]TicketResponse, 0, len(issued))
	for index := range issued {
		rows = append(rows, toTicketResponse(issued[index]))
	}
	httpx.WriteJSON(response, http.StatusOK, TicketListEnvelope{Data: rows})
}

// toSeatResponse is nil for a general-admission ticket, which is what keeps
// the field absent from the JSON rather than present and empty.
func toSeatResponse(label seatingdomain.Label) *SeatResponse {
	if label.Empty() {
		return nil
	}
	return &SeatResponse{
		Section: label.Section,
		Row:     label.Row,
		Seat:    label.Seat,
		Label:   label.String(),
	}
}

func toScanResponse(result usecase.ScanResult) ScanResponse {
	response := ScanResponse{
		Outcome:        string(result.Outcome),
		Admitted:       result.Outcome.Admitted(),
		TicketTitle:    result.TicketTitle,
		Sequence:       result.Sequence,
		OrderReference: result.OrderReference,
		Remaining:      result.Remaining,
		AdmittedCount:  result.Admitted,
	}
	if result.AdmittedAt != nil {
		stamp := result.AdmittedAt.UTC().Format("2006-01-02T15:04:05Z")
		response.AdmittedAt = &stamp
	}
	response.Seat = toSeatResponse(result.Seat)
	// AdmittedBy is deliberately not sent. It is an account id, it means
	// nothing on a door screen, and the audit that needs it reads the row.
	return response
}

func toTicketResponse(item domain.Admission) TicketResponse {
	response := TicketResponse{
		ID:          item.ID,
		TicketTitle: item.TicketTitle,
		Sequence:    item.Sequence,
		Code:        item.Code.Formatted(),
		QRPayload:   item.Code.String(),
		Status:      string(item.Status),
		Seat:        toSeatResponse(item.Seat),
	}
	if item.AdmittedAt != nil {
		stamp := item.AdmittedAt.UTC().Format("2006-01-02T15:04:05Z")
		response.AdmittedAt = &stamp
	}
	return response
}

// actor is who is asking, as the use case needs it. This package decides
// nothing about access.
func actor(request *http.Request) authdomain.Actor {
	return authdomain.ActorFromContext(request.Context())
}

func eventID(request *http.Request) string {
	return strings.TrimSpace(request.PathValue("id"))
}

// statusFor maps failures onto HTTP.
//
// Note what is NOT here: a refused scan. "Already used" and "wrong event" are
// results, not errors, and they come back as 200 with an outcome. A door that
// had to distinguish a 409 from a 422 to tell a person why they cannot come in
// is a door that shows a stack trace to a queue.
func statusFor(err error) int {
	switch {
	case errors.Is(err, authdomain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, authdomain.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, eventdomain.ErrNotFound),
		errors.Is(err, orderdomain.ErrNotFound),
		errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// @Summary		Imagem do ingresso
// @Description	O QR Code de um ingresso, em PNG, para ser exibido ou impresso. Endereçado pelo ID do ingresso e não pelo código: o endereço de uma imagem aparece no histórico do navegador e nos registros de qualquer proxy, e um ID sem sessão não serve para nada. Restrito ao comprador do pedido e a administradores.
// @Tags			Entrada
// @Produce		png
// @Security		BearerAuth
// @Param		id path string true "ID do pedido"
// @Param		admissionId path string true "ID do ingresso"
// @Success		200 {file} binary
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/orders/{id}/tickets/{admissionId}/qr.png [get]
func (h *Handler) qr(response http.ResponseWriter, request *http.Request) {
	png, err := h.admissions.QR(
		request.Context(), actor(request),
		strings.TrimSpace(request.PathValue("id")),
		strings.TrimSpace(request.PathValue("admissionId")),
	)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}

	response.Header().Set("Content-Type", "image/png")
	// PRIVATE and no-store. A ticket is a bearer credential rendered as a
	// picture: a shared cache holding it would serve one buyer's entry to the
	// next person behind the same proxy.
	response.Header().Set("Cache-Control", "private, no-store, max-age=0")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(png)
}
