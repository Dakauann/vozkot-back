package webhooks

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	queuedomain "vozkot/domain/queue"
	"vozkot/infra/mercadopago"
	queueRepository "vozkot/infra/repositories/queue"
	"vozkot/infra/testsupport"
	queueUsecase "vozkot/usecases/queue"
)

const secret = "whsec_test"

type harness struct {
	mux *http.ServeMux
	db  *gorm.DB
	// paymentID is unique per test, so the jobs it produces are its own.
	paymentID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testsupport.Database(t)
	jobs := queueRepository.NewJobRepository(db)
	paymentID := strconv.FormatInt(time.Now().UnixNano()%1_000_000_000_000, 10)
	t.Cleanup(func() {
		db.Exec("DELETE FROM jobs WHERE dedupe_key = ?", queuedomain.TypeSyncPayment+":"+paymentID)
	})

	mux := http.NewServeMux()
	NewMercadoPagoHandler(jobs, queueUsecase.NewDispatcher(nil), secret, 0).Register(mux)
	return &harness{mux: mux, db: db, paymentID: paymentID}
}

func (h *harness) deliver(t *testing.T, paymentID, signature string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"id":112233,"type":"payment","action":"payment.updated","data":{"id":"` + paymentID + `"}}`
	request := httptest.NewRequest(http.MethodPost, "/webhooks/mercadopago?data.id="+paymentID+"&type=payment", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-request-id", "req-1")
	if signature != "" {
		request.Header.Set("x-signature", signature)
	}
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

// pending counts the open sync jobs for THIS test's payment only.
func (h *harness) pending(t *testing.T) int64 {
	t.Helper()
	var total int64
	if err := h.db.Raw("SELECT COUNT(*) FROM jobs WHERE dedupe_key = ? AND status = 'pending'",
		queuedomain.TypeSyncPayment+":"+h.paymentID).Scan(&total).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return total
}

func TestGenuineNotificationIsAcceptedAndQueued(t *testing.T) {
	h := newHarness(t)
	signature := mercadopago.SignForTesting(h.paymentID, "req-1", secret, time.Now())

	response := h.deliver(t, h.paymentID, signature)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", response.Code, response.Body.String())
	}
	if got := h.pending(t); got != 1 {
		t.Fatalf("pending jobs = %d, want the sync scheduled", got)
	}
}

func TestForgedNotificationIsRejectedAndNothingIsQueued(t *testing.T) {
	h := newHarness(t)
	forged := mercadopago.SignForTesting(h.paymentID, "req-1", "not-the-secret", time.Now())

	response := h.deliver(t, h.paymentID, forged)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if body := response.Body.String(); strings.Contains(strings.ToLower(body), "mismatch") {
		t.Fatalf("the response explains why the forgery failed: %s", body)
	}
	if got := h.pending(t); got != 0 {
		t.Fatalf("pending jobs = %d, want 0: a forgery must schedule nothing", got)
	}
}

func TestMissingSignatureIsRejected(t *testing.T) {
	h := newHarness(t)

	response := h.deliver(t, h.paymentID, "")

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestRedeliveriesCollapseIntoOneOpenJob(t *testing.T) {
	h := newHarness(t)

	// Mercado Pago delivers "created", "updated" and "approved" within a
	// second, and retries on top of that. One read of the final state is the
	// right amount of work.
	for i := 0; i < 5; i++ {
		signature := mercadopago.SignForTesting(h.paymentID, "req-1", secret, time.Now())
		if response := h.deliver(t, h.paymentID, signature); response.Code != http.StatusAccepted {
			t.Fatalf("delivery %d status = %d", i+1, response.Code)
		}
	}

	if got := h.pending(t); got != 1 {
		t.Fatalf("pending jobs = %d, want 1 for five deliveries of one payment", got)
	}
}

func TestNonPaymentNotificationsAreAcknowledgedAndIgnored(t *testing.T) {
	h := newHarness(t)
	signature := mercadopago.SignForTesting("55", "req-1", secret, time.Now())
	t.Cleanup(func() { h.db.Exec("DELETE FROM jobs WHERE dedupe_key = ?", queuedomain.TypeSyncPayment+":55") })

	request := httptest.NewRequest(http.MethodPost, "/webhooks/mercadopago?data.id=55&type=merchant_order",
		strings.NewReader(`{"type":"merchant_order","data":{"id":"55"}}`))
	request.Header.Set("x-request-id", "req-1")
	request.Header.Set("x-signature", signature)
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)

	// 200, not 4xx: the provider must stop retrying something that will never
	// be acted on.
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := h.pending(t); got != 0 {
		t.Fatalf("pending jobs = %d, want 0", got)
	}
}

func TestLegacyIPNFormatIsAccepted(t *testing.T) {
	h := newHarness(t)
	signature := mercadopago.SignForTesting(h.paymentID, "req-1", secret, time.Now())

	// The old format: a GET with the id in the query and no body at all.
	request := httptest.NewRequest(http.MethodGet, "/webhooks/mercadopago?topic=payment&id="+h.paymentID+"&data.id="+h.paymentID, nil)
	request.Header.Set("x-request-id", "req-1")
	request.Header.Set("x-signature", signature)
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", recorder.Code, recorder.Body.String())
	}
}
