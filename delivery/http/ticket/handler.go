package ticket

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"vozkot/delivery/http/httpx"
	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/ticket"
	usecase "vozkot/usecases/ticket"
)

// defaultPageSize keeps an unbounded listing from turning into an accidental
// full-table scan the first time the catalogue grows.
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

type Handler struct {
	service *usecase.Service
}

func NewHandler(service *usecase.Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Register(router *http.ServeMux) {
	router.HandleFunc("GET /api/v1/tickets", h.list)
	router.HandleFunc("POST /api/v1/tickets", h.create)
	router.HandleFunc("GET /api/v1/tickets/{id}", h.get)
	router.HandleFunc("PUT /api/v1/tickets/{id}", h.update)
	router.HandleFunc("DELETE /api/v1/tickets/{id}", h.delete)
	router.HandleFunc("PATCH /api/v1/tickets/{id}/status", h.changeStatus)
}

// @Summary		Listar ingressos
// @Description	Lista os lotes de ingressos DO OPERADOR AUTENTICADO, incluindo rascunhos. Um administrador vê os de todos. Aceita filtro de status, busca por título ou descrição do lote, ordenação e paginação. O parâmetro `eventId` restringe a um evento; ele não amplia o escopo, que é sempre o do chamador.
// @Tags			Ingressos
// @Produce		json
// @Security		BearerAuth
// @Param		status query string false "Status do ingresso" Enums(draft,on_sale,sold_out,cancelled)
// @Param		eventId query string false "Filtrar pelos lotes de um evento"
// @Param		q query string false "Busca por título ou descrição do lote"
// @Param		sort query string false "Ordenação" Enums(starts_at,created_at,price)
// @Param		limit query int false "Itens por página (padrão 20, máximo 100)"
// @Param		offset query int false "Deslocamento da paginação"
// @Success		200 {object} TicketListEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		500 {object} ErrorResponse
// @Router		/api/v1/tickets [get]
func (h *Handler) list(response http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	limit := intQuery(query.Get("limit"), defaultPageSize)
	if limit > maxPageSize {
		limit = maxPageSize
	}
	offset := intQuery(query.Get("offset"), 0)

	filter := domain.Filter{
		Status: domain.Status(strings.TrimSpace(query.Get("status"))),
		Query:  query.Get("q"),
		// One event's tiers, which is what managing an event asks for. The
		// repository has always supported it; only the query parameter was
		// missing, so the dashboard had to list every tier of every event and
		// narrow it in the browser.
		EventID: strings.TrimSpace(query.Get("eventId")),
		Sort:    domain.Sort(strings.TrimSpace(query.Get("sort"))),
		Limit:   limit,
		Offset:  offset,
	}
	page, err := h.service.List(request.Context(), actor(request), filter)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, TicketListEnvelope{
		Data:   toTicketResponses(page.Items),
		Total:  page.Total,
		Limit:  limit,
		Offset: offset,
	})
}

// @Summary		Criar um ingresso
// @Description	Cria um lote de ingressos para um evento. O preço é informado em centavos e o ingresso nasce como rascunho, invisível para o comprador, até ser colocado à venda.
// @Tags			Ingressos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		request body CreateRequest true "Dados do ingresso"
// @Success		201 {object} TicketEnvelope
// @Failure		400 {object} ErrorResponse
// @Failure		401 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/tickets [post]
func (h *Handler) create(response http.ResponseWriter, request *http.Request) {
	var body CreateRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	claims, _ := authdomain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	item, err := h.service.Create(request.Context(), usecase.CreateInput{
		// The owner comes from the session, never from the body: a client that
		// could name the owner could create listings in someone else's name.
		OwnerID: claims.UserID,
		// The event DOES come from the body. It was missing here, and the
		// symptom was total: every tier creation answered 422 "a ticket tier
		// must belong to an event" while the client was sending a perfectly
		// good one, because the field was read off the request and then never
		// passed on. The usecase tests all construct CreateInput directly, so
		// nothing in the suite crossed this line.
		EventID:     body.EventID,
		Title:       body.Title,
		Description: body.Description,
		PriceCents:  body.PriceCents,
		Quantity:    body.Quantity,
		Status:      domain.Status(body.Status),
	})
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	response.Header().Set("Location", "/api/v1/tickets/"+item.ID)
	httpx.WriteJSON(response, http.StatusCreated, TicketEnvelope{Data: toTicketResponse(item)})
}

// @Summary		Consultar um ingresso
// @Description	Retorna um lote de ingressos pelo identificador. A arte do evento fica no evento, não no lote.
// @Tags			Ingressos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do ingresso"
// @Success		200 {object} TicketEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/tickets/{id} [get]
func (h *Handler) get(response http.ResponseWriter, request *http.Request) {
	item, err := h.service.Get(request.Context(), actor(request), ticketID(request))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, TicketEnvelope{Data: toTicketResponse(item)})
}

// @Summary		Atualizar um ingresso
// @Description	Substitui os dados editáveis do ingresso. A quantidade não pode ficar abaixo do total já vendido e a quantidade vendida não é editável.
// @Tags			Ingressos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do ingresso"
// @Param		request body UpdateRequest true "Dados do ingresso"
// @Success		200 {object} TicketEnvelope
// @Failure		400 {object} ErrorResponse
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/tickets/{id} [put]
func (h *Handler) update(response http.ResponseWriter, request *http.Request) {
	var body UpdateRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	item, err := h.service.Update(request.Context(), actor(request), ticketID(request), usecase.UpdateInput{
		Title:       body.Title,
		Description: body.Description,
		PriceCents:  body.PriceCents,
		Quantity:    body.Quantity,
		Status:      domain.Status(body.Status),
	})
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, TicketEnvelope{Data: toTicketResponse(item)})
}

// @Summary		Excluir um ingresso
// @Description	Remove o lote de ingressos. A arte do evento não é afetada: ela pertence ao evento.
// @Tags			Ingressos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do ingresso"
// @Success		204 "Removido"
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/tickets/{id} [delete]
func (h *Handler) delete(response http.ResponseWriter, request *http.Request) {
	if err := h.service.Delete(request.Context(), actor(request), ticketID(request)); err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

// @Summary		Alterar o status de um ingresso
// @Description	Move o ingresso entre rascunho, à venda, esgotado e cancelado. Colocar à venda exige estoque disponível.
// @Tags			Ingressos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do ingresso"
// @Param		request body ChangeStatusRequest true "Novo status"
// @Success		200 {object} TicketEnvelope
// @Failure		400 {object} ErrorResponse
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/tickets/{id}/status [patch]
func (h *Handler) changeStatus(response http.ResponseWriter, request *http.Request) {
	var body ChangeStatusRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	item, err := h.service.ChangeStatus(request.Context(), actor(request), ticketID(request), domain.Status(body.Status))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, TicketEnvelope{Data: toTicketResponse(item)})
}

// actor is who is asking, as the use case needs it.
//
// The whole of this package's involvement in authorisation: translate the
// session into a value and pass it down. Deciding anything here would put the
// rule in the one layer a CLI or a job never goes through.
func actor(request *http.Request) authdomain.Actor {
	return authdomain.ActorFromContext(request.Context())
}

func ticketID(request *http.Request) string {
	return strings.TrimSpace(request.PathValue("id"))
}

func intQuery(raw string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

// statusFor maps domain failures onto HTTP. Keeping the table here is what lets
// the use cases return plain errors and stay unaware that HTTP exists.
func statusFor(err error) int {
	switch {
	case errors.Is(err, authdomain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, authdomain.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrHasOrders):
		return http.StatusConflict
	case errors.Is(err, domain.ErrInvalidEvent),
		errors.Is(err, domain.ErrInvalidTitle),
		errors.Is(err, domain.ErrInvalidPrice),
		errors.Is(err, domain.ErrInvalidQuantity),
		errors.Is(err, domain.ErrInvalidStatus),
		errors.Is(err, domain.ErrQuantityBelowSold),
		errors.Is(err, domain.ErrNoStock):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
