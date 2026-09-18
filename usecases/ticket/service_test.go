package ticket

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/ticket"
	userdomain "vozkot/domain/user"
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

	page, err := h.service.List(ctx, h.actor(), domain.Filter{})

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

	byTitle, err := h.service.List(ctx, h.actor(), domain.Filter{OwnerID: h.ownerID, Query: "open bar"})
	if err != nil || len(byTitle.Items) != 1 {
		t.Fatalf("search by title: items=%d err=%v", len(byTitle.Items), err)
	}
	byDescription, err := h.service.List(ctx, h.actor(), domain.Filter{OwnerID: h.ownerID, Query: "welcome"})
	if err != nil || len(byDescription.Items) != 1 {
		t.Fatalf("search by description: items=%d err=%v", len(byDescription.Items), err)
	}
	none, err := h.service.List(ctx, h.actor(), domain.Filter{OwnerID: h.ownerID, Query: "camarote-que-nao-existe"})
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

	updated, err := h.service.Update(ctx, h.actor(), item.ID, UpdateInput{
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

	_, err = h.service.Update(ctx, h.actor(), item.ID, UpdateInput{
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

	err = h.service.Delete(ctx, h.actor(), item.ID)

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

	if err := h.service.Delete(ctx, h.actor(), item.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if _, err := h.service.Get(ctx, h.actor(), item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get() after delete error = %v, want %v", err, domain.ErrNotFound)
	}
}

// actor is the harness's owner, as the use case now expects it.
//
// The ownership rule moved from the HTTP handler into the use case, so a test
// that drives the service directly has to say who it is, which is exactly the
// property that makes the rule reachable from a CLI or a job.
func (h *harness) actor() authdomain.Actor {
	return authdomain.Actor{ID: h.ownerID, Role: userdomain.RoleUser}
}

// stranger is a second operator with nothing of their own.
func (h *harness) stranger(t *testing.T) authdomain.Actor {
	t.Helper()
	id := testsupport.Unique("usr")
	if err := h.db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Other Operator', ?, 'x', 'user', 0, NOW(), NOW())`, id, id+"@vozkot.test").Error; err != nil {
		t.Fatalf("seed stranger: %v", err)
	}
	t.Cleanup(func() { h.db.Exec("DELETE FROM users WHERE id = ?", id) })
	return authdomain.Actor{ID: id, Role: userdomain.RoleUser}
}

// onSaleTier creates one tier owned by the harness's operator, on sale.
func (h *harness) onSaleTier(t *testing.T) *domain.Ticket {
	t.Helper()
	item, err := h.service.Create(context.Background(), h.createInput())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	live, err := h.service.ChangeStatus(context.Background(), h.actor(), item.ID, domain.StatusOnSale)
	if err != nil {
		t.Fatalf("ChangeStatus() error = %v", err)
	}
	return live
}

// One operator must never see, edit or delete another's tiers.
//
// This is the leak that prompted the refactor: an account that had never
// created an event opened the Ingressos tab and saw 601 tiers, every row
// reading "Evento não encontrado" because they belonged to box offices whose
// events it could not read. The listing was unscoped: the HTTP handler was the
// only thing that could have narrowed it, and it did not, and the by-id routes
// had no ownership check at all, so a price could be rewritten and an on-sale
// tier cancelled by anybody signed in.
//
// The rule now lives in this package, which is why this test drives it without
// an http.Request.
func TestOneOperatorCannotReachAnothersTiers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tier := h.onSaleTier(t)
	stranger := h.stranger(t)

	t.Run("the listing shows them nothing", func(t *testing.T) {
		page, err := h.service.List(ctx, stranger, domain.Filter{})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(page.Items) != 0 || page.Total != 0 {
			t.Fatalf("a stranger saw %d of %d tiers; an unscoped listing is every box office's prices and stock",
				len(page.Items), page.Total)
		}
	})

	// A filter is a request, not an authorisation.
	t.Run("naming the owner in the filter does not help", func(t *testing.T) {
		page, err := h.service.List(ctx, stranger, domain.Filter{OwnerID: h.ownerID})
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("a stranger listed %d tiers by naming the owner", len(page.Items))
		}
	})

	t.Run("reading one is refused", func(t *testing.T) {
		if _, err := h.service.Get(ctx, stranger, tier.ID); !errors.Is(err, authdomain.ErrForbidden) {
			t.Fatalf("Get() error = %v, want ErrForbidden", err)
		}
	})

	t.Run("repricing is refused", func(t *testing.T) {
		_, err := h.service.Update(ctx, stranger, tier.ID, UpdateInput{
			Title:      "Owned",
			PriceCents: 1,
			Quantity:   300,
			Status:     domain.StatusOnSale,
		})
		if !errors.Is(err, authdomain.ErrForbidden) {
			t.Fatalf("Update() error = %v, want ErrForbidden", err)
		}
	})

	t.Run("cancelling somebody else's sale is refused", func(t *testing.T) {
		if _, err := h.service.ChangeStatus(ctx, stranger, tier.ID, domain.StatusCancelled); !errors.Is(err, authdomain.ErrForbidden) {
			t.Fatalf("ChangeStatus() error = %v, want ErrForbidden", err)
		}
	})

	t.Run("deleting is refused", func(t *testing.T) {
		if err := h.service.Delete(ctx, stranger, tier.ID); !errors.Is(err, authdomain.ErrForbidden) {
			t.Fatalf("Delete() error = %v, want ErrForbidden", err)
		}
	})

	// The refusals protected the row rather than failing after it was written.
	t.Run("the tier is untouched", func(t *testing.T) {
		item, err := h.service.Get(ctx, h.actor(), tier.ID)
		if err != nil {
			t.Fatalf("the owner can no longer read their own tier: %v", err)
		}
		if item.PriceCents != tier.PriceCents {
			t.Errorf("price is now %d, want %d", item.PriceCents, tier.PriceCents)
		}
		if item.Status != domain.StatusOnSale {
			t.Errorf("status is now %q, want on_sale", item.Status)
		}
		if item.Title != tier.Title {
			t.Errorf("title is now %q, want %q", item.Title, tier.Title)
		}
	})
}

// An operator of the platform supports everybody, which is what makes the
// scoping above safe to be strict.
func TestAnAdministratorReachesEveryTier(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tier := h.onSaleTier(t)

	operator := h.stranger(t)
	operator.Role = userdomain.RoleAdmin

	page, err := h.service.List(ctx, operator, domain.Filter{OwnerID: h.ownerID})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Items) == 0 {
		t.Fatal("an administrator saw none of the owner's tiers")
	}
	if _, err := h.service.Get(ctx, operator, tier.ID); err != nil {
		t.Fatalf("an administrator could not read a tier: %v", err)
	}
}

// A caller with no session must not be treated as an owner of the rows that
// have no owner.
func TestAnUnauthenticatedCallerListsNothing(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.List(context.Background(), authdomain.Actor{}, domain.Filter{})
	if !errors.Is(err, authdomain.ErrUnauthorized) {
		t.Fatalf("List() with no session = %v, want ErrUnauthorized", err)
	}
}
