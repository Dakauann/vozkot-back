package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	domain "vozkot/domain/admission"
	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	orderdomain "vozkot/domain/order"
	ticketdomain "vozkot/domain/ticket"
	userdomain "vozkot/domain/user"
	admissionRepository "vozkot/infra/repositories/admission"
	eventRepository "vozkot/infra/repositories/event"
	orderRepository "vozkot/infra/repositories/order"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
)

// The door, end to end, against a real PostgreSQL.
//
// What these cover that the repository tests cannot: the ORDER of the checks.
// A scan has to refuse an unauthorised caller before it says anything about a
// code, refuse a malformed code before it queries, and refuse a code whose
// order has stopped being paid even when the admission itself still looks
// issued. Each of those is a sequencing decision in the use case, invisible
// from either side alone.

type harness struct {
	db       *gorm.DB
	service  *Service
	repo     *admissionRepository.AdmissionRepository
	ownerID  string
	buyerID  string
	eventID  string
	ticketID string
	orderID  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	testsupport.Encryption(t)
	db := testsupport.Database(t)
	ctx := context.Background()

	ownerID := seedUser(t, db, "Organizador")
	buyerID := seedUser(t, db, "Maria Souza")
	eventID := testsupport.SeedEvent(t, db, ownerID)

	tier, err := ticketdomain.New(testsupport.Unique("tkt"), ownerID, ticketdomain.Draft{
		EventID: eventID, Title: "Pista", PriceCents: 24000, Quantity: 100,
		Status: ticketdomain.StatusOnSale,
	}, time.Now())
	if err != nil {
		t.Fatalf("build tier: %v", err)
	}
	if err := ticketRepository.NewTicketRepository(db).Create(ctx, tier); err != nil {
		t.Fatalf("create tier: %v", err)
	}

	orderID := testsupport.Unique("ord")
	if err := db.Exec(`
		INSERT INTO orders (
			id, event_id, buyer_id, buyer_name, buyer_email, buyer_document,
			subtotal_cents, service_fee_cents, total_cents, currency,
			refund_policy_version, status, hold_expires_at, confirmed,
			paid_at, created_at, updated_at
		) VALUES (?, ?, ?, 'Maria Souza', 'maria@exemplo.com.br', '12345678909',
			48000, 4800, 52800, 'BRL', 1, 'paid', NOW(), true, NOW(), NOW(), NOW())`,
		orderID, eventID, buyerID).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO order_items (
			id, order_id, ticket_id, ticket_title, quantity,
			unit_price_cents, unit_fee_cents, total_cents, fee_cents, created_at
		) VALUES (?, ?, ?, 'Pista', 2, 24000, 2400, 48000, 4800, NOW())`,
		testsupport.Unique("oit"), orderID, tier.ID).Error; err != nil {
		t.Fatalf("seed order item: %v", err)
	}

	t.Cleanup(func() {
		db.Exec("DELETE FROM admissions WHERE order_id = ?", orderID)
		db.Exec("DELETE FROM order_items WHERE order_id = ?", orderID)
		db.Exec("DELETE FROM orders WHERE id = ?", orderID)
		db.Exec("DELETE FROM tickets WHERE id = ?", tier.ID)
		db.Exec("DELETE FROM users WHERE id IN (?, ?)", ownerID, buyerID)
	})

	repo := admissionRepository.NewAdmissionRepository(db)
	return &harness{
		db:   db,
		repo: repo,
		service: NewService(
			repo,
			orderRepository.NewOrderRepository(db),
			eventRepository.NewEventRepository(db),
		),
		ownerID: ownerID, buyerID: buyerID,
		eventID: eventID, ticketID: tier.ID, orderID: orderID,
	}
}

func seedUser(t *testing.T, db *gorm.DB, name string) string {
	t.Helper()
	id := testsupport.Unique("usr")
	if err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, ?, ?, 'x', 'user', 0, NOW(), NOW())`,
		id, name, id+"@vozkot.test").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func (h *harness) door() authdomain.Actor {
	return authdomain.Actor{ID: h.ownerID, Role: userdomain.RoleUser}
}

func (h *harness) buyer() authdomain.Actor {
	return authdomain.Actor{ID: h.buyerID, Role: userdomain.RoleUser}
}

// issue mints the order's admissions the way settlement does.
func (h *harness) issue(t *testing.T) []domain.Admission {
	t.Helper()
	issued, err := h.repo.IssueForOrder(context.Background(), domain.OrderLines{
		OrderID: h.orderID,
		EventID: h.eventID,
		Lines:   []domain.Line{{TicketID: h.ticketID, TicketTitle: "Pista", Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("IssueForOrder(): %v", err)
	}
	return issued
}

func TestScanAdmitsOnceAndThenReportsTheFirstEntry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	issued := h.issue(t)
	code := issued[0].Code

	first, err := h.service.Scan(ctx, h.door(), h.eventID, code.String())
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	if first.Outcome != OutcomeAdmitted || !first.Outcome.Admitted() {
		t.Fatalf("the first scan was %q, want admitted", first.Outcome)
	}
	if first.TicketTitle != "Pista" {
		t.Errorf("the door was not told the tier: %q", first.TicketTitle)
	}
	if first.OrderReference != orderdomain.Reference(h.orderID) {
		t.Errorf("order reference = %q, want %q", first.OrderReference, orderdomain.Reference(h.orderID))
	}
	// The counters come back with the verdict so the screen stays live.
	if first.Admitted != 1 || first.Remaining != 1 {
		t.Errorf("counters after one entry = %d in, %d left; want 1 and 1", first.Admitted, first.Remaining)
	}

	// The grouped form, as a doorperson would type it off the ticket.
	second, err := h.service.Scan(ctx, h.door(), h.eventID, code.Formatted())
	if err != nil {
		t.Fatalf("the second Scan(): %v", err)
	}
	if second.Outcome != OutcomeAlreadyAdmitted {
		t.Fatalf("the second scan was %q, want already_admitted", second.Outcome)
	}
	if second.AdmittedAt == nil {
		t.Fatal("the door was not told WHEN the code was first used")
	}
	if second.AdmittedBy != h.ownerID {
		t.Errorf("AdmittedBy = %q, want the account that scanned it", second.AdmittedBy)
	}

	// The other ticket on the same order is untouched: a group of two is two
	// entries, not one.
	other, err := h.service.Scan(ctx, h.door(), h.eventID, issued[1].Code.String())
	if err != nil {
		t.Fatalf("Scan() of the second ticket: %v", err)
	}
	if other.Outcome != OutcomeAdmitted {
		t.Fatalf("the order's second ticket was %q, want admitted", other.Outcome)
	}
}

// Rubbish is refused by arithmetic, before any query runs.
func TestScanRefusesAMalformedCodeWithoutLooking(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.issue(t)

	for name, raw := range map[string]string{
		"empty":        "",
		"a sentence":   "let me in",
		"a typo":       "AAAA-AAAA-AAAA",
		"wrong length": "7QP52NFXW78",
	} {
		result, err := h.service.Scan(ctx, h.door(), h.eventID, raw)
		if err != nil {
			t.Fatalf("%s: Scan() errored: %v", name, err)
		}
		if result.Outcome != OutcomeMalformed {
			t.Errorf("%s (%q) was %q, want malformed", name, raw, result.Outcome)
		}
	}

	// A well-formed code nobody holds is a different answer, because it means
	// something different to the doorperson.
	unknown, err := domain.NewCode()
	if err != nil {
		t.Fatalf("NewCode(): %v", err)
	}
	result, err := h.service.Scan(ctx, h.door(), h.eventID, unknown.String())
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	if result.Outcome != OutcomeUnknown {
		t.Fatalf("an unissued code was %q, want unknown", result.Outcome)
	}
}

// A real ticket at the wrong door has to be directed, not accused.
func TestScanRefusesACodeFromAnotherEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	issued := h.issue(t)

	otherEvent := testsupport.SeedEvent(t, h.db, h.ownerID)
	result, err := h.service.Scan(ctx, h.door(), otherEvent, issued[0].Code.String())
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	if result.Outcome != OutcomeWrongEvent {
		t.Fatalf("a ticket for another event was %q, want wrong_event", result.Outcome)
	}

	// And it must still work at its own door afterwards: being presented at
	// the wrong stage must not spend it.
	right, err := h.service.Scan(ctx, h.door(), h.eventID, issued[0].Code.String())
	if err != nil {
		t.Fatalf("Scan() at the right door: %v", err)
	}
	if right.Outcome != OutcomeAdmitted {
		t.Fatalf("the ticket was %q at its own door after being shown at the wrong one", right.Outcome)
	}
}

// Ownership of the door is checked BEFORE the code, so this endpoint cannot be
// used to find out whether a code exists.
func TestScanRefusesACallerWhoDoesNotRunTheDoor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	issued := h.issue(t)

	stranger := authdomain.Actor{ID: seedUser(t, h.db, "Outro"), Role: userdomain.RoleUser}
	if _, err := h.service.Scan(ctx, stranger, h.eventID, issued[0].Code.String()); !errors.Is(err, authdomain.ErrForbidden) {
		t.Fatalf("Scan() by a stranger = %v, want ErrForbidden", err)
	}
	// A malformed code must be refused with the SAME error, or the difference
	// tells an unauthorised caller whether their guess parsed.
	if _, err := h.service.Scan(ctx, stranger, h.eventID, "nonsense"); !errors.Is(err, authdomain.ErrForbidden) {
		t.Fatalf("Scan() of rubbish by a stranger = %v, want ErrForbidden", err)
	}

	// The ticket must be unspent: a refused caller must not have consumed it.
	found, err := h.repo.FindByCode(ctx, issued[0].Code)
	if err != nil {
		t.Fatalf("FindByCode(): %v", err)
	}
	if found.Status != domain.StatusIssued {
		t.Fatalf("an unauthorised scan spent the ticket: %q", found.Status)
	}

	// An administrator runs every door.
	operator := authdomain.Actor{ID: seedUser(t, h.db, "Operador"), Role: userdomain.RoleAdmin}
	result, err := h.service.Scan(ctx, operator, h.eventID, issued[0].Code.String())
	if err != nil {
		t.Fatalf("Scan() by an operator: %v", err)
	}
	if result.Outcome != OutcomeAdmitted {
		t.Fatalf("an operator's scan was %q, want admitted", result.Outcome)
	}
}

// A refund withdraws entry. The admission is voided by the refund path, and
// the door says so rather than saying the ticket does not exist.
func TestScanRefusesAVoidedTicket(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	issued := h.issue(t)

	if _, err := h.repo.VoidForOrder(ctx, h.orderID); err != nil {
		t.Fatalf("VoidForOrder(): %v", err)
	}
	result, err := h.service.Scan(ctx, h.door(), h.eventID, issued[0].Code.String())
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	if result.Outcome != OutcomeVoid {
		t.Fatalf("a refunded ticket was %q, want void", result.Outcome)
	}
}

// The belt to the voiding brace: an admission that still looks issued must not
// admit anybody if the order behind it has stopped being paid.
func TestScanChecksTheOrderIsStillPaid(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	issued := h.issue(t)

	// The order moves without the admission being voided: a status corrected
	// by hand, or a bug in the refund path.
	if err := h.db.Exec("UPDATE orders SET status = 'refunded' WHERE id = ?", h.orderID).Error; err != nil {
		t.Fatalf("move the order: %v", err)
	}

	result, err := h.service.Scan(ctx, h.door(), h.eventID, issued[0].Code.String())
	if err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	if result.Outcome != OutcomeNotPaid {
		t.Fatalf("a ticket whose order is refunded was %q, want not_paid", result.Outcome)
	}

	// And it stayed unspent, so correcting the order lets it back in.
	found, err := h.repo.FindByCode(ctx, issued[0].Code)
	if err != nil {
		t.Fatalf("FindByCode(): %v", err)
	}
	if found.Status != domain.StatusIssued {
		t.Fatalf("the refusal spent the ticket: %q", found.Status)
	}
}

// The holder gets their own codes. The organiser does not, because whoever has
// a code can walk in on it.
func TestForOrderIsScopedToTheBuyer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.issue(t)

	mine, err := h.service.ForOrder(ctx, h.buyer(), h.orderID)
	if err != nil {
		t.Fatalf("ForOrder() for the buyer: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("the buyer got %d tickets, want 2", len(mine))
	}
	for _, item := range mine {
		if item.Code == "" {
			t.Fatalf("admission %s came back without its code; the holder cannot show it", item.ID)
		}
	}

	// The event's organiser runs the door but must not hold the keys.
	if _, err := h.service.ForOrder(ctx, h.door(), h.orderID); !errors.Is(err, authdomain.ErrForbidden) {
		t.Fatalf("ForOrder() for the organiser = %v, want ErrForbidden", err)
	}
	stranger := authdomain.Actor{ID: seedUser(t, h.db, "Outro"), Role: userdomain.RoleUser}
	if _, err := h.service.ForOrder(ctx, stranger, h.orderID); !errors.Is(err, authdomain.ErrForbidden) {
		t.Fatalf("ForOrder() for a stranger = %v, want ErrForbidden", err)
	}
}

func TestCountersAreScopedToTheDoor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	issued := h.issue(t)

	remaining, admitted, err := h.service.Counters(ctx, h.door(), h.eventID)
	if err != nil {
		t.Fatalf("Counters(): %v", err)
	}
	if remaining != 2 || admitted != 0 {
		t.Fatalf("Counters() = %d left, %d in; want 2 and 0", remaining, admitted)
	}

	if _, err := h.service.Scan(ctx, h.door(), h.eventID, issued[0].Code.String()); err != nil {
		t.Fatalf("Scan(): %v", err)
	}
	remaining, admitted, err = h.service.Counters(ctx, h.door(), h.eventID)
	if err != nil {
		t.Fatalf("Counters(): %v", err)
	}
	if remaining != 1 || admitted != 1 {
		t.Fatalf("Counters() after one entry = %d left, %d in; want 1 and 1", remaining, admitted)
	}

	stranger := authdomain.Actor{ID: seedUser(t, h.db, "Outro"), Role: userdomain.RoleUser}
	if _, _, err := h.service.Counters(ctx, stranger, h.eventID); !errors.Is(err, authdomain.ErrForbidden) {
		t.Fatalf("Counters() for a stranger = %v, want ErrForbidden", err)
	}
}

// A door for an event that does not exist is a 404, not a leak.
func TestScanRefusesAnUnknownEvent(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.Scan(context.Background(), h.door(), "evt_does_not_exist", "AAAA-AAAA-AAAA")
	if !errors.Is(err, eventdomain.ErrNotFound) {
		t.Fatalf("Scan() at an unknown event = %v, want ErrNotFound", err)
	}
}
