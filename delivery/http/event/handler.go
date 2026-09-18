// Package event is the transport for the catalogue: the pages a buyer browses
// and the screens an operator edits them on.
//
// The routes split in two, and the split is a security boundary rather than a
// convenience. Everything under /api/v1/public is reachable without a session
// and MUST only ever answer with published events; everything under
// /api/v1/events requires one and sees drafts. The public handlers pin
// Status to published themselves rather than trusting a query parameter,
// because a filter a client can set is a filter a client can unset.
package event

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vozkot/delivery/http/httpx"
	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/event"
	mediadomain "vozkot/domain/media"
	"vozkot/domain/pricing"
	usecase "vozkot/usecases/event"
)

// maxMultipartBytes caps the whole upload request, a little above the largest
// asset the domain accepts. The slack covers the multipart envelope itself, so
// a legal 50 MB video is not rejected for the boundary bytes around it.
const maxMultipartBytes = mediadomain.MaxVideoBytes + (1 << 20)

// cityOptions bounds the filter's own city list.
const cityOptions = 60

type Handler struct {
	service *usecase.Service
	// fee is the service charge the box office adds on top of a tier price.
	//
	// Carried here so the public event page can quote what a buyer will
	// actually pay. It is the SAME value the checkout service was built with;
	// two sources for one number is how a page ends up promising a price the
	// charge then contradicts. The zero value adds nothing, which is what a
	// deployment with no fee configured gets.
	fee pricing.Fee
}

func NewHandler(service *usecase.Service, fee pricing.Fee) *Handler {
	return &Handler{service: service, fee: fee}
}

// RegisterPublic mounts the buyer-facing routes. No session required.
func (h *Handler) RegisterPublic(router *http.ServeMux) {
	router.HandleFunc("GET /api/v1/public/events", h.listPublic)
	router.HandleFunc("GET /api/v1/public/events/{slug}", h.getPublic)
	router.HandleFunc("GET /api/v1/public/events/{id}/tiers", h.publicTiers)
	router.HandleFunc("GET /api/v1/public/filters", h.filters)
}

// RegisterProtected mounts the operator routes, which see drafts.
func (h *Handler) RegisterProtected(router *http.ServeMux) {
	router.HandleFunc("GET /api/v1/events", h.list)
	router.HandleFunc("POST /api/v1/events", h.create)
	router.HandleFunc("GET /api/v1/events/{id}", h.get)
	router.HandleFunc("PUT /api/v1/events/{id}", h.update)
	router.HandleFunc("DELETE /api/v1/events/{id}", h.delete)
	router.HandleFunc("POST /api/v1/events/{id}/publish", h.publish)
	router.HandleFunc("POST /api/v1/events/{id}/unpublish", h.unpublish)
	router.HandleFunc("POST /api/v1/events/{id}/pin", h.pin)
	router.HandleFunc("POST /api/v1/events/{id}/media", h.uploadMedia)
	router.HandleFunc("DELETE /api/v1/events/{id}/media/{mediaId}", h.deleteMedia)
}

// @Summary		Listar eventos publicados
// @Description	A vitrine pública. Aceita busca textual, categoria, cidade, intervalo de datas, preço máximo, apenas gratuitos, apenas com ingressos disponíveis, ordenação e paginação. Só retorna eventos publicados, independentemente do que o cliente enviar.
// @Tags			Catálogo
// @Produce		json
// @Param		q query string false "Busca por nome, local, cidade ou descrição"
// @Param		category query string false "Categoria"
// @Param		city query string false "Cidade"
// @Param		from query string false "Eventos a partir desta data (RFC3339)"
// @Param		until query string false "Eventos até esta data (RFC3339)"
// @Param		maxPrice query int false "Preço máximo em centavos do lote mais barato"
// @Param		free query bool false "Apenas eventos gratuitos"
// @Param		available query bool false "Apenas eventos com ingressos disponíveis"
// @Param		sort query string false "Ordenação" Enums(relevance,starts_at,price,created_at)
// @Param		limit query int false "Itens por página (padrão 24, máximo 60)"
// @Param		offset query int false "Deslocamento da paginação"
// @Success		200 {object} ListEnvelope
// @Router		/api/v1/public/events [get]
func (h *Handler) listPublic(response http.ResponseWriter, request *http.Request) {
	filter := parseFilter(request)
	// The catalogue is asked for BY NAME. The status used to be pinned here,
	// which meant transport was the thing keeping unannounced line-ups out of
	// a public response; ListPublic pins it where it cannot be widened.
	h.writeList(response, request, filter, audiencePublic)
}

// @Summary		Listar meus eventos
// @Description	Lista os eventos do operador autenticado, incluindo rascunhos.
// @Tags			Eventos
// @Produce		json
// @Security		BearerAuth
// @Success		200 {object} ListEnvelope
// @Failure		401 {object} ErrorResponse
// @Router		/api/v1/events [get]
func (h *Handler) list(response http.ResponseWriter, request *http.Request) {
	filter := parseFilter(request)
	if status := strings.TrimSpace(request.URL.Query().Get("status")); status != "" {
		filter.Status = domain.Status(status)
	}
	h.writeList(response, request, filter, audienceOperator)
}

// audience says which listing is being served, so writeList asks the use case
// for that one by name instead of assembling a filter that could mean either.
type audience int

const (
	audiencePublic audience = iota
	audienceOperator
)

func (h *Handler) writeList(
	response http.ResponseWriter,
	request *http.Request,
	filter domain.Filter,
	who audience,
) {
	filter = filter.Normalize()

	var page domain.Page
	var err error
	switch who {
	case audiencePublic:
		page, err = h.service.ListPublic(request.Context(), filter)
	default:
		page, err = h.service.List(request.Context(), actor(request), filter)
	}
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, ListEnvelope{
		Data:   toListingResponses(page.Items),
		Total:  page.Total,
		Limit:  filter.Limit,
		Offset: filter.Offset,
	})
}

// parseFilter reads the query string into a filter, ignoring anything it cannot
// understand rather than refusing the request.
//
// A listing URL is something people edit by hand, share and bookmark, and a
// stale category in a two-year-old link should show the catalogue rather than a
// 400. Normalize() bounds everything that survives.
func parseFilter(request *http.Request) domain.Filter {
	query := request.URL.Query()
	filter := domain.Filter{
		Query:         strings.TrimSpace(query.Get("q")),
		City:          strings.TrimSpace(query.Get("city")),
		Sort:          domain.Sort(strings.TrimSpace(query.Get("sort"))),
		Limit:         intQuery(query.Get("limit"), domain.DefaultPageSize),
		Offset:        intQuery(query.Get("offset"), 0),
		OnlyFree:      boolQuery(query.Get("free")),
		AvailableOnly: boolQuery(query.Get("available")),
	}
	if category := domain.Category(strings.TrimSpace(query.Get("category"))); category.Valid() {
		filter.Category = category
	}
	if from, err := time.Parse(time.RFC3339, strings.TrimSpace(query.Get("from"))); err == nil {
		filter.StartsFrom = &from
	}
	if until, err := time.Parse(time.RFC3339, strings.TrimSpace(query.Get("until"))); err == nil {
		filter.StartsUntil = &until
	}
	if raw := strings.TrimSpace(query.Get("maxPrice")); raw != "" {
		if cents, err := strconv.ParseInt(raw, 10, 64); err == nil && cents >= 0 {
			filter.MaxPriceCents = &cents
		}
	}
	return filter
}

// @Summary		Consultar um evento publicado
// @Description	A página pública do evento, resolvida pelo slug da URL. Um rascunho responde 404 e não 403: um 403 confirmaria que o evento existe naquele endereço.
// @Tags			Catálogo
// @Produce		json
// @Param		slug path string true "Slug do evento"
// @Success		200 {object} EventEnvelope
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/public/events/{slug} [get]
func (h *Handler) getPublic(response http.ResponseWriter, request *http.Request) {
	item, err := h.service.GetPublished(request.Context(), strings.TrimSpace(request.PathValue("slug")))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, EventEnvelope{Data: toEventResponse(item)})
}

// @Summary		Lotes de ingressos à venda
// @Description	Os lotes que o evento vende, do mais barato ao mais caro. Só retorna lotes à venda de eventos publicados: é o que o painel de compra da página do evento monta.
// @Tags			Catálogo
// @Produce		json
// @Param		id path string true "ID do evento"
// @Success		200 {object} TierListEnvelope
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/public/events/{id}/tiers [get]
func (h *Handler) publicTiers(response http.ResponseWriter, request *http.Request) {
	tiers, err := h.service.OnSaleTiers(request.Context(), eventID(request))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, TierListEnvelope{Data: toTierResponses(tiers, h.fee)})
}

// @Summary		Opções de filtro
// @Description	As categorias com a contagem de eventos publicados em cada uma, e as cidades com mais eventos. É o que a página de listagem usa para montar os próprios filtros.
// @Tags			Catálogo
// @Produce		json
// @Success		200 {object} FiltersEnvelope
// @Router		/api/v1/public/filters [get]
func (h *Handler) filters(response http.ResponseWriter, request *http.Request) {
	filters, err := h.service.AvailableFilters(request.Context(), cityOptions)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}

	categories := make([]CategoryResponse, 0, len(filters.Categories))
	for _, option := range filters.Categories {
		categories = append(categories, CategoryResponse{Value: string(option.Category), Count: option.Count})
	}
	cities := make([]CityResponse, 0, len(filters.Cities))
	for _, city := range filters.Cities {
		cities = append(cities, CityResponse{City: city.City, UF: city.UF, Count: city.Count})
	}
	httpx.WriteJSON(response, http.StatusOK, FiltersEnvelope{
		Data: FiltersResponse{Categories: categories, Cities: cities},
	})
}

// @Summary		Criar um evento
// @Tags			Eventos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		request body CreateRequest true "Dados do evento"
// @Success		201 {object} EventEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/events [post]
func (h *Handler) create(response http.ResponseWriter, request *http.Request) {
	var body CreateRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	claims, ok := authdomain.ClaimsFromContext(request.Context())
	if !ok || claims == nil {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	item, err := h.service.Create(request.Context(), usecase.CreateInput{
		// The owner comes from the session, never from the body: a client that
		// could name the owner could create listings in someone else's name.
		OwnerID:     claims.UserID,
		Name:        body.Name,
		Description: body.Description,
		Category:    domain.Category(strings.TrimSpace(body.Category)),
		Location:    toLocationRequest(body.Location),
		StartsAt:    body.StartsAt,
		EndsAt:      body.EndsAt,
		Status:      domain.Status(strings.TrimSpace(body.Status)),
		SalesMode:   domain.SalesMode(strings.TrimSpace(body.SalesMode)),
	})
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	response.Header().Set("Location", "/api/v1/events/"+item.ID)
	httpx.WriteJSON(response, http.StatusCreated, EventEnvelope{Data: toEventResponse(item)})
}

// @Summary		Consultar um evento
// @Tags			Eventos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Success		200 {object} EventEnvelope
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/events/{id} [get]
func (h *Handler) get(response http.ResponseWriter, request *http.Request) {
	item, err := h.service.Get(request.Context(), actor(request), eventID(request))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, EventEnvelope{Data: toEventResponse(item)})
}

// @Summary		Atualizar um evento
// @Tags			Eventos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		request body UpdateRequest true "Dados do evento"
// @Success		200 {object} EventEnvelope
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/events/{id} [put]
func (h *Handler) update(response http.ResponseWriter, request *http.Request) {
	var body UpdateRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}

	item, err := h.service.Update(request.Context(), actor(request), eventID(request), usecase.UpdateInput{
		Name:        body.Name,
		Description: body.Description,
		Category:    domain.Category(strings.TrimSpace(body.Category)),
		Location:    toLocationRequest(body.Location),
		StartsAt:    body.StartsAt,
		EndsAt:      body.EndsAt,
		Status:      domain.Status(strings.TrimSpace(body.Status)),
		SalesMode:   domain.SalesMode(strings.TrimSpace(body.SalesMode)),
	})
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, EventEnvelope{Data: toEventResponse(item)})
}

// @Summary		Excluir um evento
// @Description	Recusa com 409 se o evento ainda tiver lotes de ingressos, porque esses lotes podem ter pedidos e os pedidos são o registro do dinheiro que mudou de mãos.
// @Tags			Eventos
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Success		204 "Removido"
// @Failure		409 {object} ErrorResponse
// @Router		/api/v1/events/{id} [delete]
func (h *Handler) delete(response http.ResponseWriter, request *http.Request) {
	if err := h.service.Delete(request.Context(), actor(request), eventID(request)); err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

// @Summary		Publicar um evento
// @Tags			Eventos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Success		200 {object} EventEnvelope
// @Router		/api/v1/events/{id}/publish [post]
func (h *Handler) publish(response http.ResponseWriter, request *http.Request) {
	item, err := h.service.Publish(request.Context(), actor(request), eventID(request))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, EventEnvelope{Data: toEventResponse(item)})
}

// @Summary		Despublicar um evento
// @Tags			Eventos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Success		200 {object} EventEnvelope
// @Router		/api/v1/events/{id}/unpublish [post]
func (h *Handler) unpublish(response http.ResponseWriter, request *http.Request) {
	item, err := h.service.Unpublish(request.Context(), actor(request), eventID(request))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, EventEnvelope{Data: toEventResponse(item)})
}

// @Summary		Posicionar o pino no mapa
// @Description	Grava as coordenadas escolhidas por uma pessoa. Elas valem mais que qualquer geocodificação: uma edição posterior no endereço não as sobrescreve.
// @Tags			Eventos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		request body PinRequest true "Coordenadas"
// @Success		200 {object} EventEnvelope
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/events/{id}/pin [post]
func (h *Handler) pin(response http.ResponseWriter, request *http.Request) {
	var body PinRequest
	if err := httpx.ReadJSON(response, request, &body); err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}
	item, err := h.service.PlacePin(request.Context(), actor(request), eventID(request), body.Latitude, body.Longitude)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, EventEnvelope{Data: toEventResponse(item)})
}

// @Summary		Enviar mídia do evento
// @Description	Anexa uma imagem ou vídeo ao evento, um arquivo por requisição.
// @Tags			Eventos
// @Accept		mpfd
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		file formData file true "Arquivo"
// @Success		201 {object} MediaResponse
// @Failure		413 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/events/{id}/media [post]
func (h *Handler) uploadMedia(response http.ResponseWriter, request *http.Request) {

	request.Body = http.MaxBytesReader(response, request.Body, maxMultipartBytes)
	if err := request.ParseMultipartForm(8 << 20); err != nil {
		httpx.WriteError(response, http.StatusRequestEntityTooLarge, err)
		return
	}
	file, header, err := request.FormFile("file")
	if err != nil {
		httpx.WriteError(response, http.StatusBadRequest, errors.New("a file is required under the field name 'file'"))
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		httpx.WriteError(response, http.StatusBadRequest, err)
		return
	}

	item, err := h.service.AttachMedia(request.Context(), actor(request), eventID(request), mediadomain.Upload{
		FileName: header.Filename,
		// The browser's declared type is a hint; the use case sniffs the bytes.
		ContentType: header.Header.Get("Content-Type"),
		Data:        data,
	})
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusCreated, toMediaResponses([]mediadomain.Media{*item})[0])
}

// @Summary		Remover mídia do evento
// @Tags			Eventos
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		mediaId path string true "ID da mídia"
// @Success		204 "Removido"
// @Router		/api/v1/events/{id}/media/{mediaId} [delete]
func (h *Handler) deleteMedia(response http.ResponseWriter, request *http.Request) {
	err := h.service.RemoveMedia(request.Context(), actor(request), eventID(request), strings.TrimSpace(request.PathValue("mediaId")))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

// actor is who is asking, as the use case needs it.
//
// This package's entire involvement in authorisation: translate the session
// into a value and pass it down. The ownership rule lives in usecases/event,
// where every caller reaches it, including the seeder, which never builds an
// http.Request.
func actor(request *http.Request) authdomain.Actor {
	return authdomain.ActorFromContext(request.Context())
}

func eventID(request *http.Request) string {
	return strings.TrimSpace(request.PathValue("id"))
}

func intQuery(raw string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return parsed
}

func boolQuery(raw string) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
	return err == nil && parsed
}

// statusFor maps domain failures onto HTTP, so the use cases can return plain
// errors and stay unaware that HTTP exists.
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, authdomain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, authdomain.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, mediadomain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrHasTickets),
		errors.Is(err, domain.ErrSlugTaken),
		errors.Is(err, mediadomain.ErrLimitReached):
		return http.StatusConflict
	case errors.Is(err, mediadomain.ErrFileTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, domain.ErrInvalidName),
		errors.Is(err, domain.ErrInvalidCategory),
		errors.Is(err, domain.ErrInvalidVenue),
		errors.Is(err, domain.ErrInvalidCity),
		errors.Is(err, domain.ErrInvalidStartsAt),
		errors.Is(err, domain.ErrEndsBeforeStarts),
		errors.Is(err, domain.ErrInvalidStatus),
		errors.Is(err, domain.ErrInvalidLocation),
		errors.Is(err, mediadomain.ErrUnsupportedType),
		errors.Is(err, mediadomain.ErrEmptyFile):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
