package webhooks

import (
	"io"
	"log"
	"net/http"
	"time"

	"vozkot/delivery/http/httpx"
	"vozkot/domain/queue"
	"vozkot/infra/asaas"
	queueUsecase "vozkot/usecases/queue"
)

// AsaasHandler receives Asaas payment notifications.
//
// Same shape as the Mercado Pago one, and deliberately so: authenticate, write a
// job, answer. It never fetches the payment, never touches an order and never
// moves stock, because a webhook endpoint that does real work times out under a
// burst, and a provider that times out retries, which makes the burst worse.
//
// The one difference is authentication. Asaas sends a SHARED TOKEN rather than a
// signature over the body, so the body proves nothing about its own contents.
// That changes nothing downstream only because this integration already refuses
// to trust a notification's contents: the payment id is the single thing taken
// from it, and the state is always re-read from the API.
type AsaasHandler struct {
	jobs       queue.Queue
	dispatcher *queueUsecase.Dispatcher
	token      string
	now        func() time.Time
	newID      func() string
}

func NewAsaasHandler(jobs queue.Queue, dispatcher *queueUsecase.Dispatcher, token string) *AsaasHandler {
	return &AsaasHandler{
		jobs:       jobs,
		dispatcher: dispatcher,
		token:      token,
		now:        time.Now,
		newID:      randomID,
	}
}

func (h *AsaasHandler) Register(router *http.ServeMux) {
	router.HandleFunc("POST /webhooks/asaas", h.handle)
}

// @Summary		Webhook do Asaas
// @Description	Recebe as notificações de cobrança do Asaas. A requisição é autenticada pelo token compartilhado do cabeçalho asaas-access-token e o processamento é assíncrono: o endpoint apenas enfileira a releitura da cobrança e responde. O corpo da notificação NUNCA é usado como fonte de verdade: o Asaas envia a cobrança inteira, e mesmo assim o estado é sempre relido pela API, porque o token não assina o corpo.
// @Tags			Webhooks
// @Accept		json
// @Produce		json
// @Param		asaas-access-token header string true "Token compartilhado configurado no painel do Asaas"
// @Success		202 {object} map[string]string "Notificação aceita para processamento"
// @Success		200 {object} map[string]string "Notificação ignorada (não é de cobrança)"
// @Failure		400 {object} map[string]string
// @Failure		401 {object} map[string]string
// @Router		/webhooks/asaas [post]
func (h *AsaasHandler) handle(response http.ResponseWriter, request *http.Request) {
	// Authenticated BEFORE the body is read. A caller that cannot present the
	// token should not be able to make this process parse a megabyte of JSON.
	if err := asaas.VerifyToken(request.Header.Get(asaas.TokenHeader), h.token); err != nil {
		// Logged, never returned: a sender must not learn which part of a
		// forgery was wrong.
		log.Printf("webhook: rejected asaas notification: %v", err)
		httpx.WriteJSON(response, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, maxWebhookBody))
	if err != nil {
		httpx.WriteJSON(response, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}

	notification, err := asaas.ParseNotification(body)
	if err != nil {
		// Nothing to act on and nothing a retry would fix, so it is accepted
		// and dropped rather than redelivered forever.
		log.Printf("webhook: unusable asaas notification: %v", err)
		httpx.WriteJSON(response, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if !notification.Actionable() {
		httpx.WriteJSON(response, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	paymentID := notification.PaymentID()
	payload, err := queue.NewPayload(queue.SyncPaymentPayload{
		PaymentID: paymentID,
		EventID:   notification.DeliveryID(),
	})
	if err != nil {
		httpx.WriteJSON(response, http.StatusInternalServerError, map[string]string{"error": "could not schedule processing"})
		return
	}

	job := &queue.Job{
		ID:          "job_" + h.newID(),
		Type:        queue.TypeSyncPayment,
		Payload:     payload,
		RunAt:       h.now(),
		MaxAttempts: queue.DefaultMaxAttempts,
		// One open sync per payment, exactly as the Mercado Pago path does. A
		// provider that delivers CREATED, CONFIRMED and RECEIVED within a second
		// produces one read of the final state instead of three reads of a
		// moving one.
		DedupeKey: queue.TypeSyncPayment + ":" + paymentID,
	}

	added, err := h.jobs.Enqueue(request.Context(), job)
	if err != nil {
		httpx.WriteJSON(response, http.StatusInternalServerError, map[string]string{"error": "could not schedule processing"})
		return
	}
	if added {
		h.dispatcher.Dispatch(request.Context(), job)
	}
	httpx.WriteJSON(response, http.StatusAccepted, map[string]string{"status": "accepted"})
}
