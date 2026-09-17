// Package refund is the transport for cancellations: the buyer's self-service
// request, and the organiser's inbox for the ones that need a person.
//
// Every route here is behind the session, and none of them decides anything.
// Who may ask on which grounds, and who may answer, is decided in
// usecases/refund — a rule enforced at this layer is a rule the next caller
// forgets.
package refund

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"vozkot/delivery/http/httpx"
	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/refund"
	usecase "vozkot/usecases/refund"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

type Handler struct {
	refunds *usecase.Service
}

func NewHandler(refunds *usecase.Service) *Handler {
	return &Handler{refunds: refunds}
}

// RegisterOrderRoutes mounts the two routes that hang off an order.
//
// They live under /api/v1/orders/ so they inherit the prefix the router already
// guards with the session middleware, and they are registered by this handler
// rather than by the checkout one so that everything about refunds is in one
// file.
func (h *Handler) RegisterOrderRoutes(router *http.ServeMux) {
	router.HandleFunc("GET /api/v1/orders/{id}/refund-eligibility", h.eligibility)
	router.HandleFunc("POST /api/v1/orders/{id}/refund-request", h.request)
}

// Register mounts the organiser's inbox.
func (h *Handler) Register(router *http.ServeMux) {
	router.HandleFunc("GET /api/v1/refund-requests", h.list)
	router.HandleFunc("GET /api/v1/refund-requests/{id}", h.get)
	router.HandleFunc("POST /api/v1/refund-requests/{id}/approve", h.approve)
	router.HandleFunc("POST /api/v1/refund-requests/{id}/reject", h.reject)
}

// @Summary		Consultar se um pedido pode ser cancelado
// @Description	Responde se o comprador ainda pode cancelar e receber o reembolso, até quando, e quanto receberia de volta. Quando não pode, devolve o motivo: `window_closed` (passaram os 7 dias do arrependimento, CDC art. 49), `too_close_to_event` (faltam menos de 48h para o evento), `not_paid`, `already_refunded`, `event_passed` ou `request_open`. É a mesma função que o endpoint de solicitação usa para decidir, então a tela e a regra nunca divergem.
// @Tags			Reembolsos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido"
// @Success		200 {object} EligibilityResponse
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/orders/{id}/refund-eligibility [get]
func (h *Handler) eligibility(response http.ResponseWriter, request *http.Request) {
	claims, ok := authdomain.ClaimsFromContext(request.Context())
	if !ok || claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	orderID := strings.TrimSpace(request.PathValue("id"))
	eligibility, err := h.refunds.Evaluate(
		request.Context(), orderID, actor(request), domain.ReasonBuyerWithdrawal,
	)
	if err != nil {
		writeFailure(response, err)
		return
	}
	// The request already in flight, when there is one, so the page can render
	// "pedido de reembolso em análise" instead of a refusal with no context.
	open, err := h.refunds.OpenByOrders(request.Context(), []string{orderID})
	if err != nil {
		writeFailure(response, err)
		return
	}
	var inFlight *domain.Request
	if found, ok := open[orderID]; ok {
		inFlight = &found
	}
	httpx.WriteJSON(response, http.StatusOK, toEligibility(eligibility, inFlight))
}

// @Summary		Solicitar reembolso de um pedido
// @Description	Abre um pedido de reembolso. Dentro do prazo legal de arrependimento (7 dias da compra e no mínimo 48h antes do evento) e em caso de evento cancelado, o pedido é APROVADO na hora e o estorno entra na fila: são hipóteses que o organizador não tem como recusar. Fora disso, o pedido fica pendente para o organizador decidir. O valor devolvido inclui a taxa de serviço, conforme entendimento do Procon-SP e do STJ. Pedir duas vezes devolve 409.
// @Tags			Reembolsos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido"
// @Param		request body RequestBody true "Motivo"
// @Success		201 {object} Envelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse "O motivo informado não pode ser alegado por este usuário"
// @Failure		404 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse "Já existe um pedido de reembolso em andamento"
// @Failure		422 {object} ErrorResponse "Fora do prazo, ou pedido não reembolsável"
// @Router		/api/v1/orders/{id}/refund-request [post]
func (h *Handler) request(response http.ResponseWriter, request *http.Request) {
	claims, ok := authdomain.ClaimsFromContext(request.Context())
	if !ok || claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	var body RequestBody
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, errors.New("request body must be a JSON object"))
		return
	}

	reason := domain.Reason(strings.TrimSpace(body.Reason))
	if reason == "" {
		// The overwhelmingly common case, and the one a buyer's cancel button
		// sends: they changed their mind. Defaulted rather than required so the
		// client does not have to know the vocabulary to use the feature.
		reason = domain.ReasonBuyerWithdrawal
	}

	created, err := h.refunds.Request(request.Context(), usecase.RequestInput{
		OrderID: strings.TrimSpace(request.PathValue("id")),
		Actor:   actor(request),
		Reason:  reason,
		Note:    body.Note,
	})
	if err != nil {
		writeFailure(response, err)
		return
	}
	writeRequest(response, http.StatusCreated, created, claims)
}

// @Summary		Listar pedidos de reembolso
// @Description	A caixa de entrada do organizador e o histórico do comprador. Sem `eventId`, lista os pedidos do próprio comprador; com `eventId`, lista os do evento, e exige ser o organizador dele. Pendentes primeiro.
// @Tags			Reembolsos
// @Produce		json
// @Security		BearerAuth
// @Param		eventId query string false "Pedidos de reembolso de um evento (exige ser o organizador)"
// @Param		status query string false "Filtrar por situação" Enums(pending,approved,rejected)
// @Param		open query bool false "Somente os que ainda ocupam o pedido (pendentes e aprovados)"
// @Param		limit query int false "Itens por página (padrão 20, máximo 100)"
// @Param		offset query int false "Deslocamento da paginação"
// @Success		200 {object} ListEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Router		/api/v1/refund-requests [get]
func (h *Handler) list(response http.ResponseWriter, request *http.Request) {
	claims, ok := authdomain.ClaimsFromContext(request.Context())
	if !ok || claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	query := request.URL.Query()
	limit := intQuery(query.Get("limit"), defaultPageSize)
	if limit > maxPageSize {
		limit = maxPageSize
	}
	offset := intQuery(query.Get("offset"), 0)

	filter := domain.Filter{
		EventID: strings.TrimSpace(query.Get("eventId")),
		Status:  domain.Status(strings.TrimSpace(query.Get("status"))),
		Open:    query.Get("open") == "true",
		Limit:   limit,
		Offset:  offset,
	}
	// No event named means "my own requests". Scoped HERE, where the caller's
	// identity is known, so an omitted filter can never widen into everybody's.
	if filter.EventID == "" {
		filter.BuyerID = claims.UserID
	}

	page, err := h.refunds.List(request.Context(), filter, actor(request))
	if err != nil {
		writeFailure(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, ListEnvelope{
		Data:   toResponses(page.Items, actor(request)),
		Total:  page.Total,
		Limit:  limit,
		Offset: offset,
	})
}

// @Summary		Consultar um pedido de reembolso
// @Tags			Reembolsos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido de reembolso"
// @Success		200 {object} Envelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/refund-requests/{id} [get]
func (h *Handler) get(response http.ResponseWriter, request *http.Request) {
	claims, ok := authdomain.ClaimsFromContext(request.Context())
	if !ok || claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}
	found, err := h.refunds.Get(request.Context(), strings.TrimSpace(request.PathValue("id")), actor(request))
	if err != nil {
		writeFailure(response, err)
		return
	}
	writeRequest(response, http.StatusOK, found, claims)
}

// @Summary		Aprovar um pedido de reembolso
// @Description	Autoriza o estorno e o coloca na fila durável, na mesma transação: uma aprovação sem estorno, ou um estorno sem aprovação registrada, são estados que não podem existir. Restrito ao organizador do evento e a administradores. Aprovar duas vezes devolve 409, porque a segunda é outra pessoa discordando da primeira.
// @Tags			Reembolsos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido de reembolso"
// @Param		request body DecisionBody false "Observação da decisão"
// @Success		200 {object} Envelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse "Já decidido"
// @Router		/api/v1/refund-requests/{id}/approve [post]
func (h *Handler) approve(response http.ResponseWriter, request *http.Request) {
	h.decide(response, request, true)
}

// @Summary		Recusar um pedido de reembolso
// @Description	Recusa o pedido, com observação. A recusa é registrada e não altera o pedido de compra: é o caso sobre o qual o suporte será perguntado depois, então fica guardado. Restrito ao organizador do evento e a administradores.
// @Tags			Reembolsos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido de reembolso"
// @Param		request body DecisionBody false "Observação da decisão"
// @Success		200 {object} Envelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse "Já decidido"
// @Router		/api/v1/refund-requests/{id}/reject [post]
func (h *Handler) reject(response http.ResponseWriter, request *http.Request) {
	h.decide(response, request, false)
}

func (h *Handler) decide(response http.ResponseWriter, request *http.Request, approve bool) {
	claims, ok := authdomain.ClaimsFromContext(request.Context())
	if !ok || claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	// An empty body is fine: a note is optional on an approval and merely
	// advisable on a rejection.
	var body DecisionBody
	if request.ContentLength > 0 {
		if err := httpx.ReadJSON(response, request, &body); err != nil {
			httpx.WriteError(response, http.StatusBadRequest, errors.New("request body must be a JSON object"))
			return
		}
	}

	decided, err := h.refunds.Decide(request.Context(), usecase.DecideInput{
		RequestID: strings.TrimSpace(request.PathValue("id")),
		Actor:     actor(request),
		Approve:   approve,
		Note:      body.Note,
	})
	if err != nil {
		writeFailure(response, err)
		return
	}
	writeRequest(response, http.StatusOK, decided, claims)
}

// writeRequest answers with one refund request, hiding our side of the money
// from a caller who is not entitled to it.
//
// Every endpoint that returns a Response goes through here. Four call sites
// each writing the envelope themselves is four chances to forget the
// visibility argument, and forgetting it defaults to showing our commission.
func writeRequest(
	response http.ResponseWriter,
	status int,
	item *domain.Request,
	claims *authdomain.Claims,
) {
	httpx.WriteJSON(response, status, Envelope{Data: toResponse(
		item,
		usecase.SeesPlatformShare(authdomain.ActorFrom(claims), item.BuyerID, item.RequestedBy),
	)})
}

// actor is who is asking, as the use cases need it.
func actor(request *http.Request) authdomain.Actor {
	return authdomain.ActorFromContext(request.Context())
}

func writeFailure(response http.ResponseWriter, err error) {
	httpx.WriteCodedError(response, StatusFor(err), CodeFor(err), err)
}

func intQuery(raw string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}
