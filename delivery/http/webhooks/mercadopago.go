// Package webhooks receives provider callbacks.
//
// The endpoint does as little as it possibly can: authenticate the request,
// write a job, answer. It never fetches the payment, never touches an order and
// never moves stock, because a webhook endpoint that does real work is one that
// times out under a burst — and a provider that times out retries, which makes
// the burst worse.
package webhooks

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"vozkot/delivery/http/httpx"
	"vozkot/domain/queue"
	"vozkot/infra/mercadopago"
	queueUsecase "vozkot/usecases/queue"
)

const maxWebhookBody = 1 << 20

type MercadoPagoHandler struct {
	jobs queue.Queue
	// dispatcher hands the job straight to the broker, so an approval reaches a
	// worker in milliseconds instead of at the next poll.
	dispatcher *queueUsecase.Dispatcher
	secret     string
	// tolerance bounds how old a signature may be. Zero disables the check,
	// which is the default: Mercado Pago retries a failed delivery for hours
	// without re-signing it, so a tight window would reject exactly the
	// retries that matter most.
	tolerance time.Duration
	now       func() time.Time
	newID     func() string
}

func NewMercadoPagoHandler(jobs queue.Queue, dispatcher *queueUsecase.Dispatcher, secret string, tolerance time.Duration) *MercadoPagoHandler {
	return &MercadoPagoHandler{
		jobs:       jobs,
		dispatcher: dispatcher,
		secret:     strings.TrimSpace(secret),
		tolerance:  tolerance,
		now:        time.Now,
		newID:      randomID,
	}
}

func (h *MercadoPagoHandler) Register(router *http.ServeMux) {
	router.HandleFunc("POST /webhooks/mercadopago", h.handle)
	// Mercado Pago's legacy IPN sends a GET with ?topic=payment&id=123.
	router.HandleFunc("GET /webhooks/mercadopago", h.handle)
}

// @Summary		Webhook do Mercado Pago
// @Description	Recebe as notificações de pagamento do Mercado Pago. A requisição é autenticada pela assinatura HMAC do cabeçalho x-signature e o processamento acontece de forma assíncrona: o endpoint apenas enfileira a leitura do pagamento e responde. O corpo da notificação nunca é usado como fonte de verdade — o estado é sempre relido do provedor.
// @Tags			Webhooks
// @Accept		json
// @Produce		json
// @Param		x-signature header string true "Assinatura HMAC enviada pelo Mercado Pago"
// @Param		x-request-id header string false "Identificador da entrega"
// @Param		data.id query string false "ID do pagamento (formato moderno)"
// @Param		topic query string false "Tópico da notificação (formato IPN legado)"
// @Success		202 {object} map[string]string "Notificação aceita para processamento"
// @Success		200 {object} map[string]string "Notificação ignorada (não é de pagamento)"
// @Failure		400 {object} map[string]string
// @Failure		401 {object} map[string]string
// @Router		/webhooks/mercadopago [post]
func (h *MercadoPagoHandler) handle(response http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxWebhookBody))
	if err != nil {
		httpx.WriteJSON(response, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}

	notification, err := mercadopago.ParseNotification(body, request.URL.Query())
	if err != nil {
		// Nothing to act on and nothing a retry would fix, so it is accepted
		// and dropped rather than left for the provider to redeliver forever.
		log.Printf("webhook: unusable mercadopago notification: %v", err)
		httpx.WriteJSON(response, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	// The signature is computed over the data.id QUERY parameter, but some
	// deliveries arrive without one while still being signed over the id in the
	// body. Both are offered; each is a full HMAC comparison, so an attacker
	// gains nothing from the second candidate.
	candidates := []string{strings.TrimSpace(request.URL.Query().Get("data.id")), notification.ResourceID()}
	matchedID, err := mercadopago.VerifySignatureAny(
		request.Header.Get("x-signature"),
		request.Header.Get("x-request-id"),
		candidates,
		h.secret,
		h.tolerance,
		h.now(),
	)
	if err != nil {
		// The reason is logged, never returned: a sender must not learn which
		// part of a forgery was wrong.
		log.Printf("webhook: rejected mercadopago notification: %v", err)
		httpx.WriteJSON(response, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}

	if !notification.IsPayment() {
		// Merchant orders, plans, subscriptions: acknowledged so the provider
		// stops retrying, and otherwise ignored.
		httpx.WriteJSON(response, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	paymentID := strings.TrimSpace(matchedID)
	if paymentID == "" {
		paymentID = notification.ResourceID()
	}

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
		// One open sync per payment. A provider that delivers "created",
		// "updated" and "approved" within a second produces one read of the
		// final state instead of three reads of a moving one.
		DedupeKey: queue.TypeSyncPayment + ":" + paymentID,
	}
	added, err := h.jobs.Enqueue(request.Context(), job)
	if err != nil {
		// A 5xx makes the provider redeliver, which is exactly what should
		// happen when the work could not be scheduled.
		log.Printf("webhook: enqueue sync for payment %s: %v", paymentID, err)
		httpx.WriteJSON(response, http.StatusInternalServerError, map[string]string{"error": "could not schedule processing"})
		return
	}
	// A redelivery that found its job already open publishes nothing: the
	// open job, or its flagged rerun, is the read that will happen.
	if added {
		h.dispatcher.Dispatch(request.Context(), job)
	}
	httpx.WriteJSON(response, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func randomID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return time.Now().UTC().Format("20060102150405.000000")
	}
	return hex.EncodeToString(buffer)
}
