// Package payout exposes an organiser's balance and the ledger behind it.
//
// Read-only, on purpose. Nothing here moves money: a balance is a question, and
// the answer is computed from rows that only the settle path may write. When
// payouts are executed, that will be a scheduled job and an admin action, not
// an endpoint an organiser can press.
package payout

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"vozkot/delivery/http/httpx"
	authdomain "vozkot/domain/auth"
	ledgerdomain "vozkot/domain/ledger"
	payoutUsecase "vozkot/usecases/payout"
)

type Handler struct {
	payouts *payoutUsecase.Service
}

func NewHandler(payouts *payoutUsecase.Service) *Handler { return &Handler{payouts: payouts} }

// Register mounts the organiser's financial reads behind the session guard.
func (h *Handler) Register(router *http.ServeMux) {
	router.HandleFunc("GET /api/v1/organiser/balance", h.balance)
	router.HandleFunc("GET /api/v1/organiser/ledger", h.ledger)
}

// BalanceResponse is what an organiser is owed, split the way they read it.
type BalanceResponse struct {
	// AvailableCents is payable now. It can be NEGATIVE: a refund is counted
	// the moment it happens while the sale it reverses is not yet due, so a
	// balance below zero means refunds have outrun settlements and is shown
	// rather than clamped.
	AvailableCents int64 `json:"availableCents" example:"480600"`
	// PendingCents is sales that have not reached their settlement date.
	PendingCents int64 `json:"pendingCents" example:"1200000"`
	// ReservedCents is the slice withheld past settlement against the long tail
	// of Pix reversals.
	ReservedCents int64 `json:"reservedCents" example:"53400"`
	// TotalCents is the whole claim, due or not.
	TotalCents int64  `json:"totalCents" example:"1734000"`
	Currency   string `json:"currency" example:"BRL"`
}

// EntryResponse is one movement on the balance.
type EntryResponse struct {
	ID          string    `json:"id" example:"led_ord_9f2c_1"`
	EventID     string    `json:"eventId" example:"evt_88"`
	OrderID     string    `json:"orderId" example:"ord_9f2c"`
	Kind        string    `json:"kind" enums:"sale,reserve,refund,chargeback,gateway_fee,payout,adjustment" example:"sale"`
	AmountCents int64     `json:"amountCents" example:"480600"`
	AvailableAt time.Time `json:"availableAt"`
	CreatedAt   time.Time `json:"createdAt"`
	Note        string    `json:"note,omitempty"`
}

// LedgerResponse is a page of the ledger.
type LedgerResponse struct {
	Data  []EntryResponse `json:"data"`
	Total int64           `json:"total" example:"243"`
}

// ErrorResponse is the shape every failure here takes.
type ErrorResponse struct {
	Error string `json:"error"`
}

// @Summary		Saldo do organizador
// @Description	Quanto o organizador já tem para receber, quanto ainda não venceu e quanto está retido. `availableCents` pode ser negativo: um reembolso entra no saldo na hora, enquanto a venda que ele reverte só vence depois do evento: é assim que um repasse nunca paga um ingresso que já voltou para o comprador.
// @Tags			Repasses
// @Produce		json
// @Security		BearerAuth
// @Success		200 {object} BalanceResponse
// @Failure		401 {object} ErrorResponse
// @Router			/api/v1/organiser/balance [get]
func (h *Handler) balance(response http.ResponseWriter, request *http.Request) {
	who := actor(request)
	if who.ID == "" {
		httpx.WriteError(response, http.StatusUnauthorized, errUnauthorised)
		return
	}
	balance, err := h.payouts.Balance(request.Context(), who.ID)
	if err != nil {
		httpx.WriteError(response, http.StatusInternalServerError, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, BalanceResponse{
		AvailableCents: balance.AvailableCents,
		PendingCents:   balance.PendingCents,
		ReservedCents:  balance.ReservedCents,
		TotalCents:     balance.TotalCents,
		Currency:       "BRL",
	})
}

// @Summary		Extrato do organizador
// @Description	Cada movimento do saldo, do mais recente para o mais antigo. Filtra por evento. O extrato é somente-adição: uma correção é um lançamento novo com o sinal contrário, nunca a edição de um que já foi informado.
// @Tags			Repasses
// @Produce		json
// @Security		BearerAuth
// @Param			eventId	query	string	false	"Filtra por evento"
// @Param			limit	query	int		false	"Itens por página (máx. 200)"
// @Param			offset	query	int		false	"Deslocamento"
// @Success		200 {object} LedgerResponse
// @Failure		401 {object} ErrorResponse
// @Router			/api/v1/organiser/ledger [get]
func (h *Handler) ledger(response http.ResponseWriter, request *http.Request) {
	who := actor(request)
	if who.ID == "" {
		httpx.WriteError(response, http.StatusUnauthorized, errUnauthorised)
		return
	}
	query := request.URL.Query()
	// The organiser id comes from the SESSION and never from the query, so
	// there is no parameter to tamper with to read somebody else's ledger.
	page, err := h.payouts.Entries(request.Context(), ledgerdomain.Filter{
		OrganiserID: who.ID,
		EventID:     strings.TrimSpace(query.Get("eventId")),
		Limit:       atoi(query.Get("limit")),
		Offset:      atoi(query.Get("offset")),
	})
	if err != nil {
		httpx.WriteError(response, http.StatusInternalServerError, err)
		return
	}
	items := make([]EntryResponse, 0, len(page.Items))
	for _, entry := range page.Items {
		items = append(items, EntryResponse{
			ID:          entry.ID,
			EventID:     entry.EventID,
			OrderID:     entry.OrderID,
			Kind:        string(entry.Kind),
			AmountCents: entry.AmountCents,
			AvailableAt: entry.AvailableAt,
			CreatedAt:   entry.CreatedAt,
			Note:        entry.Note,
		})
	}
	httpx.WriteJSON(response, http.StatusOK, LedgerResponse{Data: items, Total: page.Total})
}

func actor(request *http.Request) authdomain.Actor {
	return authdomain.ActorFromContext(request.Context())
}

func atoi(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return parsed
}

// errUnauthorised is a request with no session. The middleware normally stops
// these; this is the belt to its braces, because the alternative to a guard
// here is returning an empty balance that reads as "you have earned nothing".
var errUnauthorised = errUnauth{}

type errUnauth struct{}

func (errUnauth) Error() string { return "sign in to see your balance" }
