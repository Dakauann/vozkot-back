package notification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	eventdomain "vozkot/domain/event"
	domain "vozkot/domain/notification"
	orderdomain "vozkot/domain/order"
	paymentdomain "vozkot/domain/payment"
	queuedomain "vozkot/domain/queue"
	ticketdomain "vozkot/domain/ticket"
	"vozkot/infra/config"
	"vozkot/infra/notifications"
	queueRepository "vozkot/infra/repositories/queue"
	"vozkot/infra/testsupport"
	queueUsecase "vozkot/usecases/queue"
)

// Nothing here is stubbed except the provider's HTTP endpoint, which is the
// same boundary the payment tests stub Mercado Pago at. The job table is real
// PostgreSQL, the claim is a real FOR UPDATE, the templates are the ones that
// ship, and the request that reaches the fake Resend is the request the real
// one would receive.

type sentEmail struct {
	Subject string   `json:"subject"`
	Html    string   `json:"html"`
	To      []string `json:"to"`
	From    string   `json:"from"`
}

type harness struct {
	db        *gorm.DB
	jobs      *queueRepository.JobRepository
	purchases *Purchases
	worker    *queueUsecase.Worker

	mu   sync.Mutex
	sent []sentEmail

	requests atomic.Int32
	// status is what the fake provider answers with; 0 means accept.
	status atomic.Int32
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testsupport.Database(t)
	h := &harness{db: db, jobs: queueRepository.NewJobRepository(db)}

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		h.requests.Add(1)
		response.Header().Set("Content-Type", "application/json")
		if status := int(h.status.Load()); status != 0 {
			response.WriteHeader(status)
			_, _ = response.Write([]byte(`{"message":"provider says no"}`))
			return
		}
		var email sentEmail
		raw, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(raw, &email)
		h.mu.Lock()
		h.sent = append(h.sent, email)
		h.mu.Unlock()
		_, _ = response.Write([]byte(`{"id":"sent"}`))
	}))
	t.Cleanup(server.Close)

	cfg := config.NotificationsConfig{
		ResendAPIKey: "re_test_key",
		FromEmail:    "no-reply@tickets.test",
		FromName:     "Vozko Tickets",
		MaxRPS:       50,
		Brand: config.BrandConfig{
			Name:         "Vozko Tickets",
			LegalName:    "Vozko Tecnologia LTDA",
			SiteURL:      "https://tickets.test",
			SupportEmail: "suporte@tickets.test",
			LogoURL:      "https://tickets.test/brand/logo.png",
		},
	}
	renderer, err := notifications.NewRenderer(cfg.Brand)
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	messenger := NewService(renderer, notifications.NewEmailSender(cfg, notifications.WithBaseURL(server.URL)))
	notifier := NewNotifier(h.jobs, queueUsecase.NewDispatcher(nil), messenger.Channels()...)
	h.purchases = NewPurchases(notifier, cfg.Brand.SiteURL)

	h.worker = queueUsecase.NewWorker(h.jobs, queueUsecase.WithJobTimeout(20*time.Second))
	h.worker.Handle(queuedomain.TypeSendNotification, func(ctx context.Context, job queuedomain.Job) error {
		payload, err := queueUsecase.Decode[domain.Payload](job)
		if err != nil {
			return err
		}
		return messenger.Deliver(ctx, payload)
	})
	return h
}

// run delivers exactly the named job, the way a broker message does. Naming it
// keeps this package's worker off rows other packages are creating in the same
// shared database.
func (h *harness) run(t *testing.T, jobID string) {
	t.Helper()
	if err := h.worker.ProcessMessage(context.Background(), queuedomain.Message{
		JobID: jobID, Type: queuedomain.TypeSendNotification,
	}); err != nil {
		t.Fatalf("process %s: %v", jobID, err)
	}
}

type jobRow struct {
	Status    string
	Attempts  int
	LastError string
	RunAt     time.Time
}

func (h *harness) row(t *testing.T, jobID string) jobRow {
	t.Helper()
	var row jobRow
	if err := h.db.Raw(`SELECT status, attempts, last_error, run_at FROM jobs WHERE id = ?`, jobID).
		Scan(&row).Error; err != nil {
		t.Fatalf("read job %s: %v", jobID, err)
	}
	return row
}

func (h *harness) countFor(t *testing.T, orderID string) int64 {
	t.Helper()
	var total int64
	if err := h.db.Raw(`SELECT COUNT(*) FROM jobs WHERE type = ? AND dedupe_key LIKE ?`,
		queuedomain.TypeSendNotification, "%:"+orderID).Scan(&total).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return total
}

func (h *harness) delivered() []sentEmail {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sentEmail(nil), h.sent...)
}

// order builds a paid order and its tier. Both are plain domain values: what is
// under test is the message they produce and the row it lands in, and neither
// needs the order persisted.
func (h *harness) order(t *testing.T) (*orderdomain.Order, *ticketdomain.Ticket, *eventdomain.Event) {
	t.Helper()
	orderID := testsupport.Unique("ord")
	t.Cleanup(func() {
		h.db.Exec(`DELETE FROM jobs WHERE type = ? AND dedupe_key LIKE ?`,
			queuedomain.TypeSendNotification, "%:"+orderID)
	})

	paidAt := time.Date(2026, time.September, 14, 1, 4, 0, 0, time.UTC)
	item := &orderdomain.Order{
		ID:         orderID,
		EventID:    "evt_1",
		BuyerName:  "Maria Souza",
		BuyerEmail: "maria@exemplo.com.br",
		Items: []orderdomain.Item{
			{TicketID: "tkt_1", TicketTitle: "Pista", Quantity: 2, UnitPriceCents: 24000, TotalCents: 48000},
		},
		TotalCents:      48000,
		Currency:        "BRL",
		Status:          orderdomain.StatusPaid,
		HoldExpiresAt:   time.Now().Add(30 * time.Minute),
		PaymentProvider: paymentdomain.ProviderMercadoPago,
		PaymentID:       "9000000001",
		PaymentMethod:   paymentdomain.MethodPix,
		PixCopyPaste:    "00020126580014BR.GOV.BCB.PIX0136exemplo-de-chave-pix5204000053039865802BR",
		PaidAt:          &paidAt,
	}
	tier := &ticketdomain.Ticket{
		ID:         "tkt_1",
		EventID:    "evt_1",
		Title:      "Pista",
		PriceCents: 24000,
		Currency:   "BRL",
	}
	happening := &eventdomain.Event{
		ID:       "evt_1",
		Name:     "Festival Aurora",
		StartsAt: time.Date(2026, time.October, 4, 1, 0, 0, 0, time.UTC),
		Location: eventdomain.Location{Venue: "Arena Vozko", City: "São Paulo"},
	}
	return item, tier, happening
}

// The whole path, end to end: a settled order raises a message, the row lands
// in PostgreSQL, a worker claims it, the shipped template renders it, and the
// provider receives an email a buyer could act on.
func TestAPaidOrderReachesTheBuyer(t *testing.T) {
	h := newHarness(t)
	item, tier, happening := h.order(t)
	ctx := context.Background()

	job, err := h.purchases.OrderPaid(ctx, h.jobs, item, tier, happening)
	if err != nil {
		t.Fatalf("raise receipt: %v", err)
	}
	if job == nil {
		t.Fatal("no job was written for a paid order")
	}
	if got := h.row(t, job.ID).Status; got != string(queuedomain.StatusPending) {
		t.Fatalf("job status = %q, want pending", got)
	}

	h.run(t, job.ID)

	if got := h.row(t, job.ID).Status; got != string(queuedomain.StatusDone) {
		t.Fatalf("job status after delivery = %q, want done", got)
	}
	sent := h.delivered()
	if len(sent) != 1 {
		t.Fatalf("provider received %d email(s), want 1", len(sent))
	}
	email := sent[0]
	if len(email.To) != 1 || email.To[0] != "maria@exemplo.com.br" {
		t.Errorf("to = %v", email.To)
	}
	if !strings.Contains(email.Subject, "Pagamento confirmado") {
		t.Errorf("subject = %q", email.Subject)
	}
	// Everything a buyer needs to recognise the purchase without opening
	// anything else.
	for _, want := range []string{
		"Festival Aurora", "Pista", "Arena Vozko", "São Paulo",
		"R$ 480,00", "R$ 240,00", "PIX",
		"sábado, 3 de outubro de 2026, 22h00",
		"13/09/2026 22:04",
		"https://tickets.test/pedidos/" + item.ID,
		"Vozko Tecnologia LTDA",
	} {
		if !strings.Contains(email.Html, want) {
			t.Errorf("email body is missing %q", want)
		}
	}
	if strings.Contains(email.Html, "<no value>") {
		t.Error("the email rendered a missing key")
	}
}

// The PIX instructions carry the code the buyer has to paste, and are raised
// only once the provider has actually issued one.
func TestPendingChargeCarriesThePixCode(t *testing.T) {
	h := newHarness(t)
	item, tier, happening := h.order(t)
	item.Status = orderdomain.StatusPendingPayment
	item.PaidAt = nil
	ctx := context.Background()

	job, err := h.purchases.ChargeIssued(ctx, h.jobs, item, tier, happening)
	if err != nil {
		t.Fatalf("raise instructions: %v", err)
	}
	h.run(t, job.ID)

	sent := h.delivered()
	if len(sent) != 1 {
		t.Fatalf("provider received %d email(s), want 1", len(sent))
	}
	if !strings.Contains(sent[0].Html, item.PixCopyPaste) {
		t.Error("the PIX copy-and-paste code is not in the email")
	}
	if !strings.Contains(sent[0].Subject, "conclua o pagamento") {
		t.Errorf("subject = %q", sent[0].Subject)
	}
}

// A webhook redelivered five times settles the same order five times. Exactly
// one receipt is queued, and the database is what decides that.
func TestTheSameEventQueuesOneMessage(t *testing.T) {
	h := newHarness(t)
	item, tier, happening := h.order(t)
	ctx := context.Background()

	first, err := h.purchases.OrderPaid(ctx, h.jobs, item, tier, happening)
	if err != nil || first == nil {
		t.Fatalf("first raise: job=%v err=%v", first, err)
	}
	for attempt := 0; attempt < 4; attempt++ {
		duplicate, err := h.purchases.OrderPaid(ctx, h.jobs, item, tier, happening)
		if err != nil {
			t.Fatalf("duplicate raise: %v", err)
		}
		if duplicate != nil {
			t.Fatalf("duplicate %d wrote a second job (%s)", attempt, duplicate.ID)
		}
	}
	if got := h.countFor(t, item.ID); got != 1 {
		t.Fatalf("%d job(s) for the order, want 1", got)
	}
}

// A provider outage must cost a delay, never a receipt. The job goes back to
// pending with a later run_at, and the buyer's confirmation arrives when the
// provider does.
func TestAProviderOutageOnlyDelaysTheMessage(t *testing.T) {
	h := newHarness(t)
	item, tier, happening := h.order(t)
	ctx := context.Background()

	h.status.Store(http.StatusServiceUnavailable)
	job, err := h.purchases.OrderPaid(ctx, h.jobs, item, tier, happening)
	if err != nil {
		t.Fatalf("raise receipt: %v", err)
	}
	h.run(t, job.ID)

	row := h.row(t, job.ID)
	if row.Status != string(queuedomain.StatusPending) {
		t.Fatalf("job status = %q, want pending for another attempt", row.Status)
	}
	if row.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", row.Attempts)
	}
	if !row.RunAt.After(time.Now().UTC()) {
		t.Fatalf("run_at = %v, want a backoff into the future", row.RunAt)
	}
	if row.LastError == "" {
		t.Error("the failure was not recorded on the job")
	}

	// The provider comes back; the same job delivers.
	h.status.Store(0)
	h.db.Exec(`UPDATE jobs SET run_at = NOW() WHERE id = ?`, job.ID)
	h.run(t, job.ID)

	if got := h.row(t, job.ID).Status; got != string(queuedomain.StatusDone) {
		t.Fatalf("job status after recovery = %q, want done", got)
	}
	if got := len(h.delivered()); got != 1 {
		t.Fatalf("provider received %d email(s), want 1", got)
	}
}

// A message that can never be delivered is parked on the FIRST attempt. Twenty
// tries over an hour would tell nobody anything the first one did not, and
// would hide work that needs a person inside the retry queue.
func TestAnUndeliverableMessageIsParkedImmediately(t *testing.T) {
	h := newHarness(t)
	item, tier, happening := h.order(t)
	ctx := context.Background()

	job, err := h.purchases.OrderPaid(ctx, h.jobs, item, tier, happening)
	if err != nil {
		t.Fatalf("raise receipt: %v", err)
	}
	// A payload naming a channel nothing can deliver, which is what a message
	// queued before a channel was retired looks like.
	h.db.Exec(`UPDATE jobs SET payload = jsonb_set(payload, '{channel}', '"carrier-pigeon"') WHERE id = ?`, job.ID)

	h.run(t, job.ID)

	row := h.row(t, job.ID)
	if row.Status != string(queuedomain.StatusDead) {
		t.Fatalf("job status = %q, want dead", row.Status)
	}
	if row.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1; an undeliverable message must not burn the budget", row.Attempts)
	}
	if h.requests.Load() != 0 {
		t.Error("the provider was called for an undeliverable message")
	}
}

// With no channel registered, no RESEND_API_KEY in development, nothing is
// queued at all, so the dead list does not fill with work that was never
// possible.
func TestNothingIsQueuedWithoutAChannel(t *testing.T) {
	h := newHarness(t)
	item, tier, happening := h.order(t)

	silent := NewPurchases(NewNotifier(h.jobs, queueUsecase.NewDispatcher(nil)), "https://tickets.test")
	job, err := silent.OrderPaid(context.Background(), h.jobs, item, tier, happening)
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if job != nil {
		t.Fatal("a job was written for a channel with no sender")
	}
	if got := h.countFor(t, item.ID); got != 0 {
		t.Fatalf("%d job(s) written, want none", got)
	}
}

// A nil Purchases is the "notifications are off" value, and every call site
// relies on it being safe rather than guarding.
func TestANilPurchasesIsSafe(t *testing.T) {
	var off *Purchases
	job, err := off.OrderPaid(context.Background(), nil, &orderdomain.Order{ID: "ord_1"}, nil, nil)
	if job != nil || err != nil {
		t.Fatalf("OrderPaid on a nil Purchases = (%v, %v), want (nil, nil)", job, err)
	}
	job, err = off.ChargeIssued(context.Background(), nil, &orderdomain.Order{ID: "ord_1"}, nil, nil)
	if job != nil || err != nil {
		t.Fatalf("ChargeIssued on a nil Purchases = (%v, %v), want (nil, nil)", job, err)
	}
}

// An order whose buyer has no address is skipped, not failed: it must never
// take a settlement down with it.
func TestAnOrderWithoutAnAddressIsSkipped(t *testing.T) {
	h := newHarness(t)
	item, tier, happening := h.order(t)
	item.BuyerEmail = ""

	job, err := h.purchases.OrderPaid(context.Background(), h.jobs, item, tier, happening)
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if job != nil {
		t.Fatal("a job was written for a buyer with no address")
	}
}

// A tier deleted while an order was open costs the event details, not the
// receipt. The buyer still gets the money, the order number and a way back in.
func TestAReceiptSurvivesAMissingTier(t *testing.T) {
	h := newHarness(t)
	item, _, _ := h.order(t)
	ctx := context.Background()

	job, err := h.purchases.OrderPaid(ctx, h.jobs, item, nil, nil)
	if err != nil || job == nil {
		t.Fatalf("raise: job=%v err=%v", job, err)
	}
	h.run(t, job.ID)

	sent := h.delivered()
	if len(sent) != 1 {
		t.Fatalf("provider received %d email(s), want 1", len(sent))
	}
	if strings.Contains(sent[0].Html, "<no value>") {
		t.Error("a missing tier rendered as \"<no value>\"")
	}
	if !strings.Contains(sent[0].Html, "R$ 480,00") {
		t.Error("the total is missing from the receipt")
	}
}
