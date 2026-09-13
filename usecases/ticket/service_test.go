package ticket

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	mediadomain "vozkot/domain/media"
	domain "vozkot/domain/ticket"
	mediaRepository "vozkot/infra/repositories/media"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/storage"
	"vozkot/infra/testsupport"
	mediaUsecase "vozkot/usecases/media"
)

// PostgreSQL for the rows, and the real local-disk adapter for the bytes: the
// same code paths production runs when Cloudflare R2 is not configured. The
// only thing a test double would add here is a way for these tests to pass
// while the product fails.

// pngBytes is a one-pixel PNG: enough for the upload path to have real content
// without embedding a fixture file.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89,
}

type harness struct {
	service *Service
	root    string
	ownerID string
	db      *gorm.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testsupport.Database(t)
	root := t.TempDir()

	files, err := storage.NewLocal(root, "http://localhost:8080/media")
	if err != nil {
		t.Fatalf("local storage: %v", err)
	}
	library := mediaUsecase.NewService(mediaRepository.NewMediaRepository(db), files)

	ownerID := testsupport.Unique("usr")
	if err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Ticket Test', ?, 'x', 'user', 0, NOW(), NOW())`, ownerID, ownerID+"@vozkot.test").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		// Tickets cascade to media rows; the user row is removed last.
		db.Exec("DELETE FROM tickets WHERE owner_id = ?", ownerID)
		db.Exec("DELETE FROM users WHERE id = ?", ownerID)
	})

	return &harness{
		service: NewService(ticketRepository.NewTicketRepository(db), library),
		root:    root,
		ownerID: ownerID,
		db:      db,
	}
}

func (h *harness) createInput() CreateInput {
	return CreateInput{
		OwnerID:    h.ownerID,
		EventName:  testsupport.Unique("Festival Aurora"),
		Title:      "Pista",
		Venue:      "Arena Castelão",
		City:       "Fortaleza, CE",
		StartsAt:   time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC),
		PriceCents: 18000,
		Quantity:   300,
	}
}

// stored reports whether a storage key exists on disk.
func (h *harness) stored(key string) bool {
	_, err := os.Stat(filepath.Join(h.root, filepath.FromSlash(key)))
	return err == nil
}

// objects counts the files under the storage root.
func (h *harness) objects(t *testing.T) int {
	t.Helper()
	total := 0
	err := filepath.WalkDir(h.root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			total++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk storage: %v", err)
	}
	return total
}

func TestCreateStoresOwnerAndEmptyGallery(t *testing.T) {
	h := newHarness(t)

	item, err := h.service.Create(context.Background(), h.createInput())

	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if item.OwnerID != h.ownerID {
		t.Fatalf("owner = %q, want %q", item.OwnerID, h.ownerID)
	}
	if item.Media == nil {
		t.Fatal("media = nil, want an empty slice so clients never receive null")
	}
}

func TestListHydratesGalleriesAndTotal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	first, err := h.service.Create(ctx, h.createInput())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	second := h.createInput()
	second.Title = "Camarote"
	second.StartsAt = first.StartsAt.Add(48 * time.Hour)
	if _, err := h.service.Create(ctx, second); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := h.service.AttachMedia(ctx, first.ID, mediadomain.Upload{
		FileName: "capa.png", ContentType: "image/png", Data: pngBytes,
	}); err != nil {
		t.Fatalf("AttachMedia() error = %v", err)
	}

	page, err := h.service.List(ctx, domain.Filter{OwnerID: h.ownerID})

	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("total = %d, items = %d, want 2 and 2", page.Total, len(page.Items))
	}
	// Default order is by door date, so the nearer event leads.
	if page.Items[0].ID != first.ID {
		t.Fatalf("first item = %q, want %q (listing must lead with the next event)", page.Items[0].ID, first.ID)
	}
	if len(page.Items[0].Media) != 1 {
		t.Fatalf("gallery size = %d, want 1: List must hydrate media, not leave it to the client", len(page.Items[0].Media))
	}
}

func TestAttachMediaRejectsUnknownTicket(t *testing.T) {
	h := newHarness(t)

	_, err := h.service.AttachMedia(context.Background(), "tkt_missing", mediadomain.Upload{
		FileName: "capa.png", ContentType: "image/png", Data: pngBytes,
	})

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("AttachMedia() error = %v, want %v", err, domain.ErrNotFound)
	}
	if h.objects(t) != 0 {
		t.Fatal("an upload for a ticket that does not exist must not reach storage")
	}
}

func TestAttachMediaRejectsUnsupportedType(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	item, err := h.service.Create(ctx, h.createInput())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	_, err = h.service.AttachMedia(ctx, item.ID, mediadomain.Upload{
		FileName: "contrato.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7"),
	})

	if !errors.Is(err, mediadomain.ErrUnsupportedType) {
		t.Fatalf("AttachMedia() error = %v, want %v", err, mediadomain.ErrUnsupportedType)
	}
	if h.objects(t) != 0 {
		t.Fatal("a rejected upload must not leave bytes in storage")
	}
}

func TestRemoveMediaIsScopedToItsTicket(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner, _ := h.service.Create(ctx, h.createInput())
	other, _ := h.service.Create(ctx, h.createInput())
	asset, err := h.service.AttachMedia(ctx, owner.ID, mediadomain.Upload{
		FileName: "capa.png", ContentType: "image/png", Data: pngBytes,
	})
	if err != nil {
		t.Fatalf("AttachMedia() error = %v", err)
	}

	if err := h.service.RemoveMedia(ctx, other.ID, asset.ID); !errors.Is(err, mediadomain.ErrNotFound) {
		t.Fatalf("RemoveMedia() across tickets error = %v, want %v", err, mediadomain.ErrNotFound)
	}
	if !h.stored(asset.StorageKey) {
		t.Fatal("a refused removal must leave the object in place")
	}
	if err := h.service.RemoveMedia(ctx, owner.ID, asset.ID); err != nil {
		t.Fatalf("RemoveMedia() error = %v", err)
	}
	if h.stored(asset.StorageKey) {
		t.Fatal("removing media must delete its object too")
	}
}

func TestDeleteClearsTheGallery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	item, _ := h.service.Create(ctx, h.createInput())
	if _, err := h.service.AttachMedia(ctx, item.ID, mediadomain.Upload{
		FileName: "capa.png", ContentType: "image/png", Data: pngBytes,
	}); err != nil {
		t.Fatalf("AttachMedia() error = %v", err)
	}

	if err := h.service.Delete(ctx, item.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if h.objects(t) != 0 {
		t.Fatalf("stored objects = %d, want 0: deleting a ticket must not orphan its assets", h.objects(t))
	}
	if _, err := h.service.Get(ctx, item.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get() after delete error = %v, want %v", err, domain.ErrNotFound)
	}
}

func TestDeleteRefusesATicketWithOrders(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	item, _ := h.service.Create(ctx, h.createInput())
	if err := h.db.Exec(`
		INSERT INTO orders (id, ticket_id, buyer_id, buyer_name, buyer_email, quantity, unit_price_cents,
		                    total_cents, currency, status, hold_expires_at, created_at, updated_at)
		VALUES (?, ?, ?, 'Maria', 'maria@exemplo.com.br', 1, 18000, 18000, 'BRL', 'paid', NOW(), NOW(), NOW())`,
		testsupport.Unique("ord"), item.ID, h.ownerID).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM jobs WHERE payload->>'orderId' IN (SELECT id FROM orders WHERE ticket_id = ?)", item.ID)
		h.db.Exec("DELETE FROM orders WHERE ticket_id = ?", item.ID)
	})

	// A tier with money behind it is the record of that money. The foreign key
	// is RESTRICT, and the API must translate that into something an operator
	// can act on rather than a 500.
	err := h.service.Delete(ctx, item.ID)

	if !errors.Is(err, domain.ErrHasOrders) {
		t.Fatalf("Delete() error = %v, want %v", err, domain.ErrHasOrders)
	}
}

func TestUpdateRewritesTheEditableFields(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	item, _ := h.service.Create(ctx, h.createInput())

	updated, err := h.service.Update(ctx, item.ID, UpdateInput{
		EventName:  "Festival Aurora",
		Title:      "Camarote",
		Venue:      "Arena Castelão",
		City:       "Fortaleza, CE",
		StartsAt:   item.StartsAt,
		PriceCents: 35000,
		Quantity:   120,
		Status:     domain.StatusOnSale,
	})

	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.Title != "Camarote" || updated.PriceCents != 35000 || updated.Status != domain.StatusOnSale {
		t.Fatalf("update did not apply: %+v", updated)
	}
}
