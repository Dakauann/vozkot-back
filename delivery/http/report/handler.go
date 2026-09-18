// Package report is the transport for what an organiser knows about their
// buyers: the aggregate dashboard, the attendee list, and the export.
//
// Every route is behind the session and every one is scoped to an event the
// caller owns: the check lives in usecases/report, not here, so that no future
// route can be added that forgets it.
//
// Every amount these routes return is the ORGANISER'S: the face value they
// priced. The platform's commission and the gross the buyer paid are not here,
// and nothing in this package removes them: domain/report does not carry them
// and the queries behind it do not select them. Do not add them to a response
// struct: see the money note in domain/report for why the absence, rather than
// a role check, is the control.
package report

import (
	"net/http"
	"strconv"
	"strings"

	"vozkot/delivery/http/httpx"
	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/report"
	usecase "vozkot/usecases/report"
)

type Handler struct {
	reports *usecase.Service
}

func NewHandler(reports *usecase.Service) *Handler {
	return &Handler{reports: reports}
}

// Register mounts the organiser routes under the event they belong to.
func (h *Handler) Register(router *http.ServeMux) {
	router.HandleFunc("GET /api/v1/events/{id}/report", h.sales)
	// The organiser's own portfolio. Under /organiser/ rather than /events/
	// because it belongs to the account, not to any one show.
	router.HandleFunc("GET /api/v1/organiser/report", h.portfolio)
	router.HandleFunc("GET /api/v1/events/{id}/attendees", h.attendees)
	router.HandleFunc("GET /api/v1/events/{id}/attendees.csv", h.export)
}

// @Summary		Relatório de vendas do evento
// @Description	Os números do evento para o organizador: totais, e a divisão por sexo, faixa etária, estado, cidade, lote e dia. Ninguém é identificado aqui: são contagens sobre os dados que o comprador informou no cadastro, congelados no momento da compra. Todo valor aqui é `netCents`: o que o organizador recebe, ou seja, o valor de face que ele definiu. A taxa de serviço da plataforma e o total pago pelo comprador não são retornados neste relatório. Restrito ao organizador do evento e a administradores.
// @Tags			Relatórios
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Success		200 {object} SalesEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/events/{id}/report [get]
func (h *Handler) sales(response http.ResponseWriter, request *http.Request) {
	if !actor(request).Authenticated() {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	sales, err := h.reports.Sales(
		request.Context(), strings.TrimSpace(request.PathValue("id")),
		actor(request),
	)
	if err != nil {
		httpx.WriteError(response, StatusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, SalesEnvelope{Data: toSalesResponse(sales)})
}

// @Summary		Listar quem comprou
// @Description	A lista de participantes, uma linha por lote de cada pedido. Traz nome, e-mail, o documento MASCARADO, e o que o comprador informou de sexo, idade, cidade e estado. O documento completo, a data de nascimento e o telefone nunca são devolvidos: o organizador precisa conferir quem chega na porta, não se passar por ela. Por padrão lista apenas pedidos pagos; use `status=all` para incluir os estornados.
// @Tags			Relatórios
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		ticketId query string false "Filtrar por lote"
// @Param		status query string false "Situação do pedido (padrão: paid)" Enums(paid,refunded,all)
// @Param		q query string false "Buscar por nome ou e-mail"
// @Param		limit query int false "Itens por página (padrão 50, máximo 200)"
// @Param		offset query int false "Deslocamento da paginação"
// @Success		200 {object} AttendeeListEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/events/{id}/attendees [get]
func (h *Handler) attendees(response http.ResponseWriter, request *http.Request) {
	if !actor(request).Authenticated() {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	filter := parseFilter(request)
	page, err := h.reports.Attendees(request.Context(), filter, actor(request))
	if err != nil {
		httpx.WriteError(response, StatusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, AttendeeListEnvelope{
		Data:   toAttendees(page.Items),
		Total:  page.Total,
		Limit:  filter.Normalize().Limit,
		Offset: filter.Offset,
	})
}

// @Summary		Exportar quem comprou (CSV)
// @Description	Baixa a lista de participantes como uma planilha. Aceita os mesmos filtros da listagem. O arquivo é UTF-8 com BOM e separado por ponto e vírgula, que é o que o Excel em português abre corretamente sem nenhum passo de importação; os valores vão em reais com vírgula decimal. As linhas são transmitidas em lotes conforme são lidas, então exportar um estádio inteiro não carrega tudo na memória.
// @Tags			Relatórios
// @Produce		text/csv
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		ticketId query string false "Filtrar por lote"
// @Param		status query string false "Situação do pedido (padrão: paid)" Enums(paid,refunded,all)
// @Param		q query string false "Buscar por nome ou e-mail"
// @Success		200 {string} string "CSV"
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/events/{id}/attendees.csv [get]
func (h *Handler) export(response http.ResponseWriter, request *http.Request) {
	if !actor(request).Authenticated() {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}

	// Authorised BEFORE a single header is written. Once the status line is
	// out, a failure can no longer be reported as one: the browser has already
	// been told the download started, and the only way left to signal an error
	// is a truncated file. So the ownership check happens through a probe read
	// that fetches nothing, and the streaming write begins only after it.
	filter := parseFilter(request)
	if _, err := h.reports.Attendees(request.Context(), probe(filter), actor(request)); err != nil {
		httpx.WriteError(response, StatusFor(err), err)
		return
	}

	writer := newCSVWriter(response)
	happening, err := h.reports.Export(request.Context(), filter, actor(request), writer.Write)
	if err != nil {
		if !writer.Started() {
			httpx.WriteError(response, StatusFor(err), err)
			return
		}
		// Mid-stream. The status is long gone and the bytes are partly
		// delivered, so the only honest thing left is to stop writing and let
		// the client see a short file; a trailing "error" row would be parsed
		// as data by every spreadsheet that opens it.
		return
	}
	_ = writer.Flush(happening)
}

// probe is the filter reduced to "fetch nothing, but resolve the event".
//
// One row would be wasted work and zero is not a legal limit, so the offset is
// pushed past any plausible result instead: the query is planned, the event is
// resolved and authorised, and no rows come back.
func probe(filter domain.AttendeeFilter) domain.AttendeeFilter {
	filter.Limit = 1
	filter.Offset = 0
	filter.Query = ""
	return filter
}

func parseFilter(request *http.Request) domain.AttendeeFilter {
	query := request.URL.Query()
	filter := domain.AttendeeFilter{
		EventID:  strings.TrimSpace(request.PathValue("id")),
		TicketID: strings.TrimSpace(query.Get("ticketId")),
		Status:   strings.TrimSpace(query.Get("status")),
		Query:    strings.TrimSpace(query.Get("q")),
		Offset:   intQuery(query.Get("offset"), 0),
	}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		filter.Limit = intQuery(raw, domain.DefaultPageSize)
	}
	return filter
}

func intQuery(raw string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

// actor is who is asking, as the use case needs it.
//
// This package decides nothing about access: usecases/report owns the
// ownership rule, and this only supplies the identity it judges.
func actor(request *http.Request) authdomain.Actor {
	return authdomain.ActorFromContext(request.Context())
}

// @Summary		Relatório geral do organizador
// @Description	Os mesmos números do relatório de evento, somados sobre TODOS os eventos do organizador: totais, ticket médio, e a divisão por sexo, faixa etária, estado, cidade, lote e dia. Ninguém é identificado: são contagens sobre o que o comprador informou no cadastro, congelado no momento da compra. O escopo vem da sessão: não existe parâmetro de organizador, portanto não há o que adulterar para ler a carteira de outra pessoa.
// @Tags			Relatórios
// @Produce		json
// @Security		BearerAuth
// @Success		200 {object} SalesEnvelope
// @Failure		401 {object} ErrorResponse
// @Router		/api/v1/organiser/report [get]
func (h *Handler) portfolio(response http.ResponseWriter, request *http.Request) {
	if !actor(request).Authenticated() {
		httpx.WriteError(response, http.StatusUnauthorized, authdomain.ErrUnauthorized)
		return
	}
	sales, err := h.reports.Portfolio(request.Context(), actor(request))
	if err != nil {
		httpx.WriteError(response, StatusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, SalesEnvelope{Data: toSalesResponse(sales)})
}
