package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vozkot/delivery/http/httpx"
	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	idempotencydomain "vozkot/domain/idempotency"
	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	ticketdomain "vozkot/domain/ticket"
	checkoutUsecase "vozkot/usecases/checkout"
	eventUsecase "vozkot/usecases/event"
	paymentUsecase "vozkot/usecases/payment"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
	// idempotencyScope namespaces keys per endpoint, so the same key used for a
	// checkout and for something else cannot collide.
	idempotencyScope = "checkout"
)

type Handler struct {
	checkout *checkoutUsecase.Service
	payments *paymentUsecase.Service
	// events resolves the night an order is for, so a listing can name the show
	// rather than an id. Optional: without it an order still renders, minus its
	// event block.
	events *eventUsecase.Service
	replay *httpx.Idempotency
}

func NewHandler(
	checkout *checkoutUsecase.Service,
	payments *paymentUsecase.Service,
	events *eventUsecase.Service,
	store idempotencydomain.Store,
	lease time.Duration,
) *Handler {
	handler := &Handler{checkout: checkout, payments: payments, events: events}
	handler.replay = httpx.NewIdempotency(store, StatusFor,
		httpx.WithLease(lease),
		// Recovery for a claim orphaned by a crash. The order table already
		// carries the key under a unique index, so the order a dead request
		// committed can be found and replayed, which is the difference
		// between a buyer seeing the tickets they bought and a buyer
		// reserving a second batch of them.
		// Failures on this endpoint are ones a buyer can act on, holding too
		// much already, or somebody else taking the last ones, and each wants
		// a different way out on screen.
		httpx.WithCodes(CodeFor),
		httpx.WithRecovery(func(ctx context.Context, key string) (httpx.Result, bool, error) {
			item, err := checkout.FindByIdempotencyKey(ctx, key)
			if errors.Is(err, orderdomain.ErrNotFound) {
				return httpx.Result{}, false, nil
			}
			if err != nil {
				return httpx.Result{}, false, err
			}
			return httpx.Result{
				Status: http.StatusCreated,
				Body:   OrderEnvelope{Data: toOrderResponse(item)},
			}, true, nil
		}),
	)
	return handler
}

func (h *Handler) Register(router *http.ServeMux) {
	router.HandleFunc("POST /api/v1/checkout", h.create)
	router.HandleFunc("POST /api/v1/orders/{id}/confirm", h.confirm)
	router.HandleFunc("GET /api/v1/orders", h.list)
	router.HandleFunc("GET /api/v1/orders/{id}", h.get)
	router.HandleFunc("POST /api/v1/orders/{id}/cancel", h.cancel)
	router.HandleFunc("POST /api/v1/orders/{id}/refund", h.refund)
}

// @Summary		Comprar ingressos
// @Description	Reserva os ingressos e abre um pedido aguardando pagamento. A reserva é imediata; a cobrança PIX é criada por um job e aparece em `payment` poucos instantes depois, então o cliente deve consultar o pedido até `payment.pixCopyPaste` ser preenchido. O cabeçalho Idempotency-Key é obrigatório: repetir a mesma requisição com a mesma chave devolve o mesmo pedido, sem reservar de novo.
// @Tags			Compras
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		Idempotency-Key header string true "Chave de idempotência do cliente (UUID)"
// @Param		request body CheckoutRequest true "Dados da compra"
// @Success		201 {object} OrderEnvelope
// @Failure		400 {object} ErrorResponse
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse "Chave reutilizada, requisição em andamento ou ingressos esgotados"
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/checkout [post]
func (h *Handler) create(response http.ResponseWriter, request *http.Request) {
	claims, _ := authdomain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}
	key := strings.TrimSpace(request.Header.Get(httpx.IdempotencyHeader))

	h.replay.Execute(response, request, idempotencyScope, func(ctx context.Context, body []byte) (httpx.Result, error) {
		var payload CheckoutRequest
		if err := json.Unmarshal(body, &payload); err != nil {
			return httpx.Result{}, errBadRequest{errors.New("request body must be a JSON object")}
		}

		item, err := h.checkout.Start(ctx, checkoutUsecase.StartInput{
			Items:          payload.Lines(),
			Confirm:        payload.Confirm,
			BuyerID:        claims.UserID,
			BuyerName:      payload.Buyer.Name,
			BuyerEmail:     payload.Buyer.Email,
			BuyerDocument:  payload.Buyer.Document,
			Method:         paymentdomain.Method(strings.TrimSpace(payload.Method)),
			IdempotencyKey: key,
		})
		if err != nil {
			return httpx.Result{}, err
		}
		return httpx.Result{Status: http.StatusCreated, Body: OrderEnvelope{Data: h.withEvent(ctx, item)}}, nil
	})
}

// @Summary		Confirmar os dados do comprador
// @Description	Registra nome, e-mail e documento do comprador, estende a reserva para a janela de pagamento e solicita a cobrança PIX. Reservar e confirmar são passos separados porque os ingressos saem do estoque assim que o comprador chega ao formulário, e não depois de preenchê-lo. Confirmar duas vezes atualiza os dados sem estender a reserva de novo nem criar uma segunda cobrança.
// @Tags			Compras
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido"
// @Param		request body ConfirmRequest true "Dados do comprador"
// @Success		200 {object} OrderEnvelope
// @Failure		400 {object} ErrorResponse
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse "A reserva expirou"
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/orders/{id}/confirm [post]
func (h *Handler) confirm(response http.ResponseWriter, request *http.Request) {
	claims, _ := authdomain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	var payload ConfirmRequest
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, errors.New("request body must be a JSON object"))
		return
	}

	item, err := h.checkout.Confirm(request.Context(), checkoutUsecase.ConfirmInput{
		OrderID:       request.PathValue("id"),
		BuyerID:       claims.UserID,
		BuyerName:     payload.Buyer.Name,
		BuyerEmail:    payload.Buyer.Email,
		BuyerDocument: payload.Buyer.Document,
		Method:        paymentdomain.Method(strings.TrimSpace(payload.Method)),
	})
	if err != nil {
		writeFailure(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, OrderEnvelope{Data: h.withEvent(request.Context(), item)})
}

// @Summary		Listar pedidos
// @Description	Lista os pedidos do comprador autenticado, do mais recente para o mais antigo. Aceita filtro por status e por ingresso.
// @Tags			Compras
// @Produce		json
// @Security		BearerAuth
// @Param		status query string false "Status do pedido" Enums(pending_payment,paid,expired,cancelled,failed,refunded,refund_required)
// @Param		ticketId query string false "Filtrar por ingresso"
// @Param		eventId query string false "Filtrar pelos pedidos de um evento"
// @Param		limit query int false "Itens por página (padrão 20, máximo 100)"
// @Param		offset query int false "Deslocamento da paginação"
// @Success		200 {object} OrderListEnvelope
// @Failure		401 {object} ErrorResponse
// @Router		/api/v1/orders [get]
func (h *Handler) list(response http.ResponseWriter, request *http.Request) {
	claims, _ := authdomain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	query := request.URL.Query()
	limit := intQuery(query.Get("limit"), defaultPageSize)
	if limit > maxPageSize {
		limit = maxPageSize
	}
	offset := intQuery(query.Get("offset"), 0)

	filter := orderdomain.Filter{
		Status:   orderdomain.Status(strings.TrimSpace(query.Get("status"))),
		TicketID: strings.TrimSpace(query.Get("ticketId")),
		// Every order for one night, which is what an organiser looking at
		// their own event asks for, and what a buyer asking "what did I buy
		// for this show" asks for too. The buyer scoping below still applies.
		EventID: strings.TrimSpace(query.Get("eventId")),
		Limit:   limit,
		Offset:  offset,
	}
	// An operator sees the whole box office; a buyer sees only their own
	// orders. Scoping here rather than in the use case keeps the rule where the
	// caller's identity is known.
	if claims.Role != string(adminRole) {
		filter.BuyerID = claims.UserID
	}

	page, err := h.checkout.List(request.Context(), filter)
	if err != nil {
		writeFailure(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, OrderListEnvelope{
		Data:   h.withEvents(request.Context(), page.Items),
		Total:  page.Total,
		Limit:  limit,
		Offset: offset,
	})
}

// @Summary		Consultar um pedido
// @Description	Retorna um pedido com o estado atual do pagamento. É este endpoint que o cliente consulta enquanto espera o código PIX aparecer.
// @Tags			Compras
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido"
// @Success		200 {object} OrderEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/orders/{id} [get]
func (h *Handler) get(response http.ResponseWriter, request *http.Request) {
	item, err := h.authorized(request)
	if err != nil {
		writeFailure(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, OrderEnvelope{Data: h.withEvent(request.Context(), item)})
}

// @Summary		Cancelar um pedido
// @Description	Desiste de um pedido aguardando pagamento e devolve os ingressos ao estoque. Cancelar um pedido já cancelado devolve o mesmo pedido, sem erro.
// @Tags			Compras
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido"
// @Success		200 {object} OrderEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/orders/{id}/cancel [post]
func (h *Handler) cancel(response http.ResponseWriter, request *http.Request) {
	item, err := h.authorized(request)
	if err != nil {
		writeFailure(response, err)
		return
	}
	cancelled, err := h.checkout.Cancel(request.Context(), item.ID)
	if err != nil {
		writeFailure(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, OrderEnvelope{Data: toOrderResponse(cancelled)})
}

// @Summary		Estornar um pedido
// @Description	Agenda o estorno de um pedido pago no provedor de pagamento e devolve os ingressos ao estoque. Restrito a administradores. O estorno é processado por um job durável e confirmado relendo a cobrança no provedor, então a resposta é 202 e o pedido deve ser consultado até ficar `refunded`. Pedir o estorno duas vezes estorna uma vez.
// @Tags			Compras
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do pedido"
// @Success		202 {object} OrderEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Failure		503 {object} ErrorResponse
// @Router		/api/v1/orders/{id}/refund [post]
func (h *Handler) refund(response http.ResponseWriter, request *http.Request) {
	claims, _ := authdomain.ClaimsFromContext(request.Context())
	if claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}
	// Refunds move money out. Only an operator may ask for one.
	if claims.Role != string(adminRole) {
		httpx.WriteError(response, http.StatusForbidden, errForbidden)
		return
	}

	// Scheduled, not performed. Calling the provider on this request meant one
	// slower than the write timeout left the money refunded at Mercado Pago and
	// the order untouched here, with nothing to reconcile the two.
	id := strings.TrimSpace(request.PathValue("id"))
	item, err := h.payments.RequestRefund(request.Context(), id)
	if err != nil {
		writeFailure(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusAccepted, OrderEnvelope{Data: toOrderResponse(item)})
}

// authorized loads the order and refuses one that belongs to someone else.
func (h *Handler) authorized(request *http.Request) (*orderdomain.Order, error) {
	claims, _ := authdomain.ClaimsFromContext(request.Context())
	if claims == nil {
		return nil, authdomain.ErrUnauthorized
	}
	item, err := h.checkout.Get(request.Context(), strings.TrimSpace(request.PathValue("id")))
	if err != nil {
		return nil, err
	}
	if claims.Role != string(adminRole) && item.BuyerID != "" && item.BuyerID != claims.UserID {
		return nil, errForbidden
	}
	return item, nil
}

const adminRole = "admin"

var errForbidden = errors.New("this order belongs to another buyer")

// errBadRequest marks a malformed request so StatusFor can answer 400 without
// the handler writing the response itself.
type errBadRequest struct{ error }

// StatusFor maps every error this endpoint can produce onto HTTP.
//
// The three 409s are the interesting ones, and they are three genuinely
// different conflicts: the tickets are gone, the key was reused with a
// different body, or the first request with this key is still running.
func StatusFor(err error) int {
	var badRequest errBadRequest
	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &badRequest):
		return http.StatusBadRequest
	case errors.Is(err, authdomain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, errForbidden):
		return http.StatusForbidden
	case errors.Is(err, orderdomain.ErrNotFound), errors.Is(err, ticketdomain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ticketdomain.ErrInsufficientStock),
		errors.Is(err, ticketdomain.ErrNotOnSale),
		errors.Is(err, idempotencydomain.ErrInFlight),
		errors.Is(err, idempotencydomain.ErrRequestMismatch),
		errors.Is(err, orderdomain.ErrIdempotencyMismatch),
		// Not 429: nothing is rate limited here. The account is holding as much
		// unpaid inventory as it is allowed to, and the way out is to pay for
		// one of those orders or cancel it, a conflict with state the caller
		// owns, which is exactly what 409 says.
		errors.Is(err, orderdomain.ErrTooManyOpenOrders),
		errors.Is(err, orderdomain.ErrTooManyHeldTickets):
		return http.StatusConflict
	case errors.Is(err, paymentUsecase.ErrNotRefundable):
		return http.StatusUnprocessableEntity
	case errors.Is(err, orderdomain.ErrInvalidQuantity),
		errors.Is(err, orderdomain.ErrInvalidBuyerName),
		errors.Is(err, orderdomain.ErrInvalidBuyerEmail),
		errors.Is(err, orderdomain.ErrInvalidTicket),
		errors.Is(err, orderdomain.ErrInvalidTransition),
		errors.Is(err, paymentdomain.ErrDocumentRequired),
		errors.Is(err, paymentdomain.ErrEmailRequired),
		errors.Is(err, paymentdomain.ErrMethodUnsupported),
		errors.Is(err, paymentdomain.ErrInvalidAmount):
		return http.StatusUnprocessableEntity
	case errors.Is(err, paymentdomain.ErrNotConfigured):
		return http.StatusServiceUnavailable
	case errors.Is(err, paymentdomain.ErrChargeNotFound):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func intQuery(raw string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

// withEvents resolves the night behind a page of orders in ONE query.
//
// A listing of twenty orders spans at most twenty events and usually far fewer,
// so the ids are de-duplicated first. Reading them one order at a time is the
// N+1 that turns a dashboard into a timeout on the day somebody has a hundred
// orders.
//
// A failure here costs the page its event titles and nothing else. An orders
// list that 500s because the catalogue was briefly unavailable would be a worse
// answer than one that shows the orders.
func (h *Handler) withEvents(ctx context.Context, items []orderdomain.Order) []OrderResponse {
	responses := toOrderResponses(items)
	if h.events == nil || len(responses) == 0 {
		return responses
	}
	seen := make(map[string]struct{}, len(responses))
	ids := make([]string, 0, len(responses))
	for _, item := range responses {
		if item.EventID == "" {
			continue
		}
		if _, already := seen[item.EventID]; already {
			continue
		}
		seen[item.EventID] = struct{}{}
		ids = append(ids, item.EventID)
	}
	if len(ids) == 0 {
		return responses
	}

	page, err := h.events.List(ctx, eventdomain.Filter{IDs: ids, Limit: len(ids)})
	if err != nil {
		log.Printf("orders: could not resolve %d event(s) for a listing: %v", len(ids), err)
		return responses
	}
	found := make(map[string]*eventdomain.Event, len(page.Items))
	for index := range page.Items {
		happening := &page.Items[index].Event
		found[happening.ID] = happening
	}
	attachEvents(responses, found)
	return responses
}

// withEvent is the single-order case, kept on the same path so one order and a
// page of them can never disagree about what an order looks like.
func (h *Handler) withEvent(ctx context.Context, item *orderdomain.Order) OrderResponse {
	responses := h.withEvents(ctx, []orderdomain.Order{*item})
	return responses[0]
}

// writeFailure answers with both halves: the sentence for the buyer and the
// code the client branches on.
func writeFailure(response http.ResponseWriter, err error) {
	httpx.WriteCodedError(response, StatusFor(err), CodeFor(err), err)
}
