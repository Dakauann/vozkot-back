package ticket

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	domain "vozkot/domain/ticket"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/testsupport"
)

// A ticket here is a TIER: a price and a number of seats, belonging to an
// event. Everything about the happening: its name, venue, date, category, map
// pin and artwork; lives on the event, and is tested in usecases/event.
//
// Real PostgreSQL, because what these tests are about is stock arithmetic the
// database arbitrates.

type harness struct {
	service *Service
	ownerID string
	eventID string
	db      *gorm.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testsupport.Database(t)

	ownerID := testsupport.Unique("usr")
	if err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Ticket Test', ?, 'x', 'user', 0, NOW(), NOW())`, ownerID, ownerID+"@vozkot.test").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM tickets WHERE owner_id = ?", ownerID)
		db.Exec("DELETE FROM users WHERE id = ?", ownerID)
	})

	return &harness{
		service: NewService(ticketRepository.NewTicketRepository(db)),
		ownerID: ownerID,
		eventID: testsupport.SeedEvent(t, db, ownerID),
		db:      db,
	}
}

func (h *harness) createInput() CreateInput {
	return CreateInput{
		OwnerID:    h.ownerID,
		EventID:    h.eventID,
		Title:      "Pista",
		PriceCents: 18000,
		Quantity:   300,
	}
}

func TestCreateStartsAsADraftInBRL(t *testing.T) {
	h := newHarness(t)

	item, err := h.service.Create(context.Background(), h.createInput())

	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if item.OwnerID != h.ownerID || item.EventID != h.eventID {
		t.Fatalf("owner = %q, event = %q, want %q and %q", item.OwnerID, item.EventID, h.ownerID, h.eventID)
	}
	if item.Status != domain.StatusDraft {
		t.Fatalf("status = %q, want a new tier to be invisible until someone publishes it", item.Status)
	}
	if item.Currency != domain.DefaultCurrency {
		t.Fatalf("currency = %q, want %q", item.Currency, domain.DefaultCurrency)
	}
}

// A tier with no event cannot be listed, found or bought, so it cannot exist.
func TestCreateRefusesATierWithNoEvent(t *testing.T) {
	h := newHarness(t)
	input := h.createInput()
	input.EventID = "  "

	_, err := h.service.Create(context.Background(), input)

	if !errors.Is(err, domain.ErrInvalidEvent) {
		t.Fatalf("Create() error = %v, want %v", err, domain.ErrInvalidEvent)
	}
}

func TestListReturnsTheEventsTiersWithATotal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first, err := h.service.Create(ctx, h.createInput())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// A second tier of the SAME event, which is what Pista plus Camarote is.
	second := h.createInput()
	second.Title = "Camarote"
	second.PriceCents = first.PriceCents + 12000
	if _, err := h.service.Create(ctx, second); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	page, err := h.service.List(ctx, domain.Filter{OwnerID: h.ownerID})

	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("total = %d, items = %d, want 2 and 2", page.Total, len(page.Items))
	}
	for _, item := range page.Items {
		if item.EventID != h.eventID {
			t.Fatalf("tier %q belongs to event %q, want %q", item.ID, item.EventID, h.eventID)
		}
	}
}

// The tier's own words only. Searching for the event is the event
// repository's job, and it does it with a real full-text index.
func TestSearchMatchesTheTiersOwnWords(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	input := h.createInput()
	input.Title = "Camarote Open Bar"
	input.Description = "Inclui welcome drink"
	if _, err := h.service.Create(ctx, input); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	byTitle, err := h.service.List(ctx, domain.Filter{OwnerID: h.ownerID, Query: "open bar"})
	if err != nil || len(byTitle.Items) != 1 {
		t.Fatalf("search by title: items=%d err=%v", len(byTitle.Items), err)
	}
	byDescription, err := h.service.List(ctx, domain.Filter{OwnerID: h.ownerID, Query: "welcome"})
	if err != nil || len(byDescription.Items) != 1 {
		t.Fatalf("search by description: items=%d err=%v", len(byDescription.Items), err)
	}
	none, err := h.service.List(ctx, domain.Filter{OwnerID: h.ownerID, Query: "camarote-que-nao-existe"})
	if err != nil || len(none.Items) != 0 {
		t.Fatalf("search for nothing: items=%d err=%v", len(none.Items), err)
	}
}

func TestUpdateRewritesTheEditableFields(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	item, err := h.service.Create(ctx, h.createInput())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	updated, err := h.service.Update(ctx, item.ID, UpdateInput{
		Title:       "Camarote",
		Description: "Vista para o palco",
		PriceCents:  35000,
		Quantity:    120,
		Status:      domain.StatusOnSale,
	})

	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Title != "Camarote" || updated.PriceCents != 35000 || updated.Quantity != 120 {
		t.Fatalf("update did not land: %+v", updated)
	}
	// The event is not among the editable fields: moving a tier between events
	// would move seats somebody already bought.
	if updated.EventID != h.eventID {
		t.Fatalf("event = %q, want it unchanged at %q", updated.EventID, h.eventID)
	}
}

// Quantity may not be pushed below what is already sold or held: that would
// promise refunds the box office cannot honour.
func TestUpdateRefusesToShrinkBelowSoldAndHeld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	item, err := h.service.Create(ctx, h.createInput())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := h.db.Exec("UPDATE tickets SET sold = 40, reserved = 10 WHERE id = ?", item.ID).Error; err != nil {
		t.Fatalf("set stock: %v", err)
	}

	_, err = h.service.Update(ctx, item.ID, UpdateInput{
		Title:      item.Title,
		PriceCents: item.PriceCents,
		Quantity:   49,
		Status:     item.Status,
	})

	if !errors.Is(err, domain.ErrQuantityBelowSold) {
		t.Fatalf("Update() error = %v, want %v", err, domain.ErrQuantityBelowSold)
	}
}

func TestDeleteRefusesATierWithOrders(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	item, err := h.service.Create(ctx, h.createInput())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	orderID := testsupport.Unique("ord")
	err = h.db.Exec(`
		INSERT INTO orders (id, event_id, buyer_id, buyer_name, buyer_email, buyer_document,
			total_cents, currency, status, hold_expires_at, confirmed,
			payment_provider, payment_id, payment_status, payment_method,
			pix_copy_paste, pix_qr_code_base64, created_at, updated_at)
		VALUES (?, ?, ?, 'Maria', 'maria@exemplo.com.br', '12345678909',
			18000, 'BRL', 'paid', NOW() + INTERVAL '30 minutes', true,
			'mercadopago', '', '', 'pix', '', '', NOW(), NOW())`,
		orderID, h.eventID, h.ownerID).Error
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	// The line is what actually names the tier now, and it is the line's
	// RESTRICT that this test is about.
	err = h.db.Exec(`
		INSERT INTO order_items (id, order_id, ticket_id, ticket_title, quantity, unit_price_cents, total_cents, created_at)
		VALUES (?, ?, ?, 'Pista', 1, 18000, 18000, NOW())`,
		testsupport.Unique("oi"), orderID, item.ID).Error
	if err != nil {
		t.Fatalf("seed order item: %v", err)
	}
	t.Cleanup(func() { h.db.Exec("DELETE FROM orders WHERE id = ?", orderID) })

	err = h.service.Delete(ctx, item.ID)

	if !errors.Is(err, domain.ErrHasOrders) {
		t.Fatalf("Delete() error = %v, want %v: the orders are the record of money that changed hands", err, domain.ErrHasOrders)
	}
}

func TestDeleteRemovesATierWithNoOrders(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	item, err := h.service.Create(ctx, h.createInput())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := h.service.Delete(ctx, item.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := h.service.Get(ctx, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get() after delete error = %v, want %v", err, domain.ErrNotFound)
	}
}
