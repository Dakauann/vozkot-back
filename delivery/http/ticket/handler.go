package ticket

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"vozkot/delivery/http/httpx"
	authdomain "vozkot/domain/auth"
	mediadomain "vozkot/domain/media"
	domain "vozkot/domain/ticket"
	usecase "vozkot/usecases/ticket"
)

// maxMultipartBytes caps the whole upload request, a little above the largest
// asset the domain accepts. The slack covers the multipart envelope itself, so
// a legal 50 MB video is not rejected for the boundary bytes around it.
const maxMultipartBytes = mediadomain.MaxVideoBytes + (1 << 20)

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
	router.HandleFunc("POST /api/v1/tickets/{id}/media", h.uploadMedia)
	router.HandleFunc("DELETE /api/v1/tickets/{id}/media/{mediaId}", h.deleteMedia)
}

// @Summary		Listar ingressos
// @Description	Lista os ingressos do evento em ordem de data de início. Aceita filtros de status, busca textual por evento, título, local ou cidade, ordenação e paginação.
// @Tags			Ingressos
// @Produce		json
// @Security		BearerAuth
// @Param		status query string false "Status do ingresso" Enums(draft,on_sale,sold_out,cancelled)
// @Param		q query string false "Busca por evento, título, local ou cidade"
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

	page, err := h.service.List(request.Context(), domain.Filter{
		Status: domain.Status(strings.TrimSpace(query.Get("status"))),
		Query:  query.Get("q"),
		Sort:   domain.Sort(strings.TrimSpace(query.Get("sort"))),
		Limit:  limit,
		Offset: offset,
	})
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
		OwnerID:     claims.UserID,
		EventName:   body.EventName,
		Title:       body.Title,
		Description: body.Description,
		Venue:       body.Venue,
		City:        body.City,
		StartsAt:    body.StartsAt,
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
// @Description	Retorna um ingresso pelo identificador, com a galeria de imagens e vídeos anexada.
// @Tags			Ingressos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do ingresso"
// @Success		200 {object} TicketEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/tickets/{id} [get]
func (h *Handler) get(response http.ResponseWriter, request *http.Request) {
	item, err := h.service.Get(request.Context(), strings.TrimSpace(request.PathValue("id")))
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
	item, err := h.service.Update(request.Context(), strings.TrimSpace(request.PathValue("id")), usecase.UpdateInput{
		EventName:   body.EventName,
		Title:       body.Title,
		Description: body.Description,
		Venue:       body.Venue,
		City:        body.City,
		StartsAt:    body.StartsAt,
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
// @Description	Remove o ingresso e todas as mídias associadas, inclusive os arquivos no armazenamento.
// @Tags			Ingressos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do ingresso"
// @Success		204 "Removido"
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/tickets/{id} [delete]
func (h *Handler) delete(response http.ResponseWriter, request *http.Request) {
	if err := h.service.Delete(request.Context(), strings.TrimSpace(request.PathValue("id"))); err != nil {
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
	item, err := h.service.ChangeStatus(request.Context(), strings.TrimSpace(request.PathValue("id")), domain.Status(body.Status))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, TicketEnvelope{Data: toTicketResponse(item)})
}

// @Summary		Enviar uma mídia do ingresso
// @Description	Envia uma imagem ou vídeo para a galeria do ingresso via multipart. O arquivo vai para o Cloudflare R2 quando as credenciais estão configuradas. Imagens: até 10 MB (jpeg, png, webp, avif, gif). Vídeos: até 50 MB (mp4, webm, quicktime). Limite de 12 mídias por ingresso.
// @Tags			Ingressos
// @Accept		multipart/form-data
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do ingresso"
// @Param		file formData file true "Arquivo de imagem ou vídeo"
// @Success		201 {object} MediaEnvelope
// @Failure		400 {object} ErrorResponse
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse
// @Failure		413 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/tickets/{id}/media [post]
func (h *Handler) uploadMedia(response http.ResponseWriter, request *http.Request) {
	// The body is capped before it is read, so an oversized upload costs the
	// connection rather than the server's memory.
	request.Body = http.MaxBytesReader(response, request.Body, maxMultipartBytes)
	if err := request.ParseMultipartForm(maxMultipartBytes); err != nil {
		httpx.WriteError(response, http.StatusRequestEntityTooLarge, mediadomain.ErrFileTooLarge)
		return
	}
	defer func() {
		if request.MultipartForm != nil {
			_ = request.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := request.FormFile("file")
	if err != nil {
		httpx.WriteError(response, http.StatusBadRequest, errors.New("multipart field \"file\" is required"))
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		httpx.WriteError(response, http.StatusRequestEntityTooLarge, mediadomain.ErrFileTooLarge)
		return
	}

	upload := mediadomain.Upload{
		FileName:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		Data:        data,
	}
	item, err := h.service.AttachMedia(request.Context(), strings.TrimSpace(request.PathValue("id")), upload)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusCreated, MediaEnvelope{Data: toMediaResponse(item)})
}

// @Summary		Remover uma mídia do ingresso
// @Description	Remove uma imagem ou vídeo da galeria do ingresso e apaga o arquivo do armazenamento.
// @Tags			Ingressos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do ingresso"
// @Param		mediaId path string true "ID da mídia"
// @Success		204 "Removido"
// @Failure		401 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/tickets/{id}/media/{mediaId} [delete]
func (h *Handler) deleteMedia(response http.ResponseWriter, request *http.Request) {
	err := h.service.RemoveMedia(
		request.Context(),
		strings.TrimSpace(request.PathValue("id")),
		strings.TrimSpace(request.PathValue("mediaId")),
	)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
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
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, mediadomain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, mediadomain.ErrLimitReached), errors.Is(err, domain.ErrHasOrders):
		return http.StatusConflict
	case errors.Is(err, mediadomain.ErrFileTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, domain.ErrInvalidEventName),
		errors.Is(err, domain.ErrInvalidTitle),
		errors.Is(err, domain.ErrInvalidVenue),
		errors.Is(err, domain.ErrInvalidStartsAt),
		errors.Is(err, domain.ErrInvalidPrice),
		errors.Is(err, domain.ErrInvalidQuantity),
		errors.Is(err, domain.ErrInvalidStatus),
		errors.Is(err, domain.ErrQuantityBelowSold),
		errors.Is(err, domain.ErrNoStock),
		errors.Is(err, mediadomain.ErrUnsupportedType),
		errors.Is(err, mediadomain.ErrEmptyFile):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}
