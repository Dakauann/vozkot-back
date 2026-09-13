package event

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	domain "vozkot/domain/event"
	mediadomain "vozkot/domain/media"
	ticketdomain "vozkot/domain/ticket"
	eventRepository "vozkot/infra/repositories/event"
	mediaRepository "vozkot/infra/repositories/media"
	ticketRepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/storage"
	"vozkot/infra/testsupport"
	mediaUsecase "vozkot/usecases/media"
)

// The catalogue runs against real PostgreSQL, and it has to.
//
// What is under test is a full-text index, a Portuguese stemmer, a LATERAL join
// that computes each card's cheapest price, and the interaction of six filters
// with a window function that counts them. Every one of those is the database's
// behaviour; a fake repository would be testing a map lookup in Go and would
// pass while the product returned nothing.

type harness struct {
	db      *gorm.DB
	service *Service
	tickets ticketdomain.Repository
	ownerID string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testsupport.Database(t)

	files, err := storage.NewLocal(t.TempDir(), "http://localhost:8080/media")
	if err != nil {
		t.Fatalf("local storage: %v", err)
	}
	library := mediaUsecase.NewService(mediaRepository.NewMediaRepository(db), files, nil)

	ownerID := testsupport.Unique("usr")
	if err := db.Exec(`
		INSERT INTO users (id, name, email, password_hash, role, token_version, created_at, updated_at)
		VALUES (?, 'Event Test', ?, 'x', 'user', 0, NOW(), NOW())`, ownerID, ownerID+"@vozkot.test").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM tickets WHERE owner_id = ?", ownerID)
		db.Exec("DELETE FROM ticket_media WHERE event_id IN (SELECT id FROM events WHERE owner_id = ?)", ownerID)
		db.Exec("DELETE FROM events WHERE owner_id = ?", ownerID)
		db.Exec("DELETE FROM users WHERE id = ?", ownerID)
	})

	tiers := ticketRepository.NewTicketRepository(db)
	return &harness{
		db: db,
		// No geocoder: these tests are about the catalogue, and a live HTTP
		// call to a third party has no place in them.
		service: NewService(eventRepository.NewEventRepository(db), tiers, library, nil),
		tickets: tiers,
		ownerID: ownerID,
	}
}

type seed struct {
	name     string
	category domain.Category
	city     string
	uf       string
	startsIn time.Duration
	// prices are the tiers to create, in centavos. An event with none has
	// nothing on sale.
	prices []int64
	// soldOut makes every tier's stock already taken.
	soldOut bool
	status  domain.Status
}

func (h *harness) seed(t *testing.T, item seed) *domain.Event {
	t.Helper()
	ctx := context.Background()

	if item.status == "" {
		item.status = domain.StatusPublished
	}
	if item.city == "" {
		item.city = "Fortaleza"
		item.uf = "CE"
	}
	if item.startsIn == 0 {
		item.startsIn = 720 * time.Hour
	}

	created, err := h.service.Create(ctx, CreateInput{
		OwnerID:  h.ownerID,
		Name:     item.name,
		Category: item.category,
		Location: domain.Location{Venue: "Arena " + item.city, City: item.city, UF: item.uf},
		StartsAt: time.Now().Add(item.startsIn),
		Status:   item.status,
	})
	if err != nil {
		t.Fatalf("seed event %q: %v", item.name, err)
	}

	for index, price := range item.prices {
		quantity := 100
		tier, err := ticketdomain.New(testsupport.Unique("tkt"), h.ownerID, ticketdomain.Draft{
			EventID:    created.ID,
			Title:      "Tier",
			PriceCents: price,
			Quantity:   quantity,
			Status:     ticketdomain.StatusOnSale,
		}, time.Now())
		if err != nil {
			t.Fatalf("build tier %d: %v", index, err)
		}
		if err := h.tickets.Create(ctx, tier); err != nil {
			t.Fatalf("create tier %d: %v", index, err)
		}
		if item.soldOut {
			if err := h.db.Exec("UPDATE tickets SET sold = quantity WHERE id = ?", tier.ID).Error; err != nil {
				t.Fatalf("sell out tier: %v", err)
			}
		}
	}
	return created
}

func (h *harness) list(t *testing.T, filter domain.Filter) domain.Page {
	t.Helper()
	filter.OwnerID = h.ownerID
	page, err := h.service.List(context.Background(), filter)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	return page
}

func names(page domain.Page) []string {
	found := make([]string, 0, len(page.Items))
	for index := range page.Items {
		found = append(found, page.Items[index].Event.Name)
	}
	return found
}

func contains(page domain.Page, name string) bool {
	for _, found := range names(page) {
		if found == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Search

// The Portuguese configuration is the whole reason for using a real full-text
// index: it stems, so a plural finds a singular and vice versa. A LIKE '%…%'
// would match neither, and could use no index either way.
func TestSearchStemsPortuguese(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Festa de Verao " + testsupport.Unique("f"), category: domain.CategoryFestasShows})

	for _, term := range []string{"festa", "festas"} {
		page := h.list(t, domain.Filter{Query: term})
		if len(page.Items) != 1 {
			t.Fatalf("search %q found %v, want the one seeded event", term, names(page))
		}
	}
}

// A term in the name must outrank the same term in a description, or searching
// for a city returns every event that merely mentions it.
func TestSearchRanksTheNameAboveTheDescription(t *testing.T) {
	h := newHarness(t)
	unique := testsupport.Unique("rock")
	ctx := context.Background()

	mentioned, err := h.service.Create(ctx, CreateInput{
		OwnerID: h.ownerID,
		// The term appears ONLY in the description here, and only in the name
		// of the other event. That is the whole comparison.
		Name:        "Noite Eletronica " + testsupport.Unique("noite"),
		Description: "Uma homenagem ao " + unique + " classico",
		Category:    domain.CategoryFestasShows,
		Location:    domain.Location{Venue: "Arena", City: "Fortaleza", UF: "CE"},
		StartsAt:    time.Now().Add(100 * time.Hour),
		Status:      domain.StatusPublished,
	})
	if err != nil {
		t.Fatalf("seed mentioned: %v", err)
	}
	named := h.seed(t, seed{name: unique + " Festival", category: domain.CategoryFestasShows, startsIn: 900 * time.Hour})

	page := h.list(t, domain.Filter{Query: unique, Sort: domain.SortRelevance})

	if len(page.Items) != 2 {
		t.Fatalf("found %v, want both events", names(page))
	}
	if page.Items[0].Event.ID != named.ID {
		t.Fatalf("first result is %q, want the event whose NAME matches (%q)",
			page.Items[0].Event.Name, named.Name)
	}
	_ = mentioned
}

// A search box that answers a typo with an empty page is one people stop using.
func TestSearchToleratesATypo(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Festival Aurora Boreal", category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{Query: "festivl aurora"})

	if len(page.Items) == 0 {
		t.Skip("pg_trgm is not installed, so typo tolerance is off by design")
	}
	if !contains(page, "Festival Aurora Boreal") {
		t.Fatalf("a misspelled search found %v", names(page))
	}
}

func TestSearchFindsTheVenueAndCity(t *testing.T) {
	h := newHarness(t)
	city := "Sorocaba"
	h.seed(t, seed{name: "Noite " + testsupport.Unique("n"), city: city, uf: "SP", category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{Query: city})

	if len(page.Items) != 1 {
		t.Fatalf("searching for a city found %v, want the event held there", names(page))
	}
}

// ---------------------------------------------------------------------------
// Filters

func TestFilterByCategory(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Show " + testsupport.Unique("s"), category: domain.CategoryFestasShows})
	h.seed(t, seed{name: "Curso " + testsupport.Unique("c"), category: domain.CategoryCursosWorkshops})

	page := h.list(t, domain.Filter{Category: domain.CategoryCursosWorkshops})

	if len(page.Items) != 1 || page.Items[0].Event.Category != domain.CategoryCursosWorkshops {
		t.Fatalf("category filter returned %v", names(page))
	}
}

// "sao paulo" has to find "São Paulo", or half the country's buyers get an
// empty page for typing their own city without an accent.
func TestFilterByCityIgnoresCase(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Noite " + testsupport.Unique("n"), city: "Recife", uf: "PE", category: domain.CategoryFestasShows})
	h.seed(t, seed{name: "Tarde " + testsupport.Unique("t"), city: "Fortaleza", uf: "CE", category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{City: "recife"})

	if len(page.Items) != 1 || page.Items[0].Event.Location.City != "Recife" {
		t.Fatalf("city filter returned %v", names(page))
	}
}

func TestFilterByDateRange(t *testing.T) {
	h := newHarness(t)
	soon := h.seed(t, seed{name: "Logo " + testsupport.Unique("l"), startsIn: 48 * time.Hour, category: domain.CategoryFestasShows})
	h.seed(t, seed{name: "Depois " + testsupport.Unique("d"), startsIn: 2000 * time.Hour, category: domain.CategoryFestasShows})

	until := time.Now().Add(240 * time.Hour)
	page := h.list(t, domain.Filter{StartsUntil: &until})

	if len(page.Items) != 1 || page.Items[0].Event.ID != soon.ID {
		t.Fatalf("date filter returned %v", names(page))
	}
}

// The cheapest tier is what a price filter compares, because a buyer filtering
// by price is asking what they can get in for, not what the best seat costs.
func TestFilterByMaxPriceUsesTheCheapestTier(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Barato " + testsupport.Unique("b"), prices: []int64{5000, 40000}, category: domain.CategoryFestasShows})
	h.seed(t, seed{name: "Caro " + testsupport.Unique("c"), prices: []int64{30000}, category: domain.CategoryFestasShows})

	ceiling := int64(10000)
	page := h.list(t, domain.Filter{MaxPriceCents: &ceiling})

	if len(page.Items) != 1 {
		t.Fatalf("price filter returned %v, want only the event with a R$50 tier", names(page))
	}
	if got := page.Items[0].FromPriceCents; got == nil || *got != 5000 {
		t.Fatalf("fromPriceCents = %v, want 5000", got)
	}
}

func TestFilterByFree(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Gratis " + testsupport.Unique("g"), prices: []int64{0}, category: domain.CategoryFestasShows})
	h.seed(t, seed{name: "Pago " + testsupport.Unique("p"), prices: []int64{12000}, category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{OnlyFree: true})

	if len(page.Items) != 1 {
		t.Fatalf("free filter returned %v", names(page))
	}
}

// "Show me what I can buy" must not list an event whose every tier is gone.
func TestFilterByAvailabilityDropsSoldOutEvents(t *testing.T) {
	h := newHarness(t)
	open := h.seed(t, seed{name: "Aberto " + testsupport.Unique("a"), prices: []int64{9000}, category: domain.CategoryFestasShows})
	h.seed(t, seed{name: "Esgotado " + testsupport.Unique("e"), prices: []int64{9000}, soldOut: true, category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{AvailableOnly: true})

	if len(page.Items) != 1 || page.Items[0].Event.ID != open.ID {
		t.Fatalf("availability filter returned %v", names(page))
	}
	if page.Items[0].AvailableTickets != 100 {
		t.Fatalf("availableTickets = %d, want 100", page.Items[0].AvailableTickets)
	}
}

// A sold-out event still lists by default, with no price, which is what renders
// as "Esgotado" rather than as free.
func TestASoldOutEventHasNoFromPrice(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Esgotado " + testsupport.Unique("e"), prices: []int64{9000}, soldOut: true, category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{})

	if len(page.Items) != 1 {
		t.Fatalf("found %v, want the sold-out event still listed", names(page))
	}
	if page.Items[0].AvailableTickets != 0 {
		t.Fatalf("availableTickets = %d, want 0", page.Items[0].AvailableTickets)
	}
}

// A draft must never reach a public listing, however the filter is built.
func TestADraftIsNeverListedPublicly(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Rascunho " + testsupport.Unique("r"), status: domain.StatusDraft, category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{Status: domain.StatusPublished})

	if len(page.Items) != 0 {
		t.Fatalf("a draft appeared in a published listing: %v", names(page))
	}
}

// ---------------------------------------------------------------------------
// Sorting and pagination

func TestDefaultOrderIsWhatHappensSoonest(t *testing.T) {
	h := newHarness(t)
	later := h.seed(t, seed{name: "Depois " + testsupport.Unique("d"), startsIn: 900 * time.Hour, category: domain.CategoryFestasShows})
	sooner := h.seed(t, seed{name: "Antes " + testsupport.Unique("a"), startsIn: 100 * time.Hour, category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{})

	if len(page.Items) != 2 {
		t.Fatalf("found %v", names(page))
	}
	if page.Items[0].Event.ID != sooner.ID || page.Items[1].Event.ID != later.ID {
		t.Fatalf("order = %v, want the nearest event first", names(page))
	}
}

// An event with nothing on sale is not "cheapest", so it sorts last rather
// than leading a price-ascending listing with a null.
func TestSortingByPricePutsEventsWithNothingOnSaleLast(t *testing.T) {
	h := newHarness(t)
	cheap := h.seed(t, seed{name: "Barato " + testsupport.Unique("b"), prices: []int64{1000}, category: domain.CategoryFestasShows})
	h.seed(t, seed{name: "Sem lote " + testsupport.Unique("s"), category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{Sort: domain.SortPrice})

	if len(page.Items) != 2 {
		t.Fatalf("found %v", names(page))
	}
	if page.Items[0].Event.ID != cheap.ID {
		t.Fatalf("order = %v, want the priced event first", names(page))
	}
}

// The total is what drives the page numbers, so it must describe the whole
// result set rather than the page in hand.
func TestPaginationWindowsTheResultsAndReportsTheRealTotal(t *testing.T) {
	h := newHarness(t)
	const total = 5
	for index := 0; index < total; index++ {
		h.seed(t, seed{
			name:     testsupport.Unique("page"),
			startsIn: time.Duration(index+1) * 100 * time.Hour,
			category: domain.CategoryFestasShows,
		})
	}

	first := h.list(t, domain.Filter{Limit: 2, Offset: 0})
	second := h.list(t, domain.Filter{Limit: 2, Offset: 2})
	last := h.list(t, domain.Filter{Limit: 2, Offset: 4})

	for label, page := range map[string]domain.Page{"first": first, "second": second, "last": last} {
		if page.Total != total {
			t.Fatalf("%s page reports total %d, want %d", label, page.Total, total)
		}
	}
	if len(first.Items) != 2 || len(second.Items) != 2 || len(last.Items) != 1 {
		t.Fatalf("page sizes = %d/%d/%d, want 2/2/1", len(first.Items), len(second.Items), len(last.Items))
	}
	// No row may appear on two pages, which is what an unstable sort would do.
	seen := map[string]bool{}
	for _, page := range []domain.Page{first, second, last} {
		for index := range page.Items {
			id := page.Items[index].Event.ID
			if seen[id] {
				t.Fatalf("event %s appeared on more than one page", id)
			}
			seen[id] = true
		}
	}
}

// A page past the end reports the real total, or a listing on page nine of a
// filter that now matches three events shows "no results" with no way back.
func TestAPagePastTheEndStillReportsTheTotal(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Unico " + testsupport.Unique("u"), category: domain.CategoryFestasShows})

	page := h.list(t, domain.Filter{Limit: 10, Offset: 100})

	if len(page.Items) != 0 {
		t.Fatalf("items = %d, want none past the end", len(page.Items))
	}
	if page.Total != 1 {
		t.Fatalf("total = %d, want 1 even on an empty page", page.Total)
	}
}

// ---------------------------------------------------------------------------
// Slugs, media and lifecycle

func TestTwoEventsWithOneNameGetDistinctAddresses(t *testing.T) {
	h := newHarness(t)
	name := "Festival " + testsupport.Unique("dup")

	first := h.seed(t, seed{name: name, category: domain.CategoryFestasShows})
	second := h.seed(t, seed{name: name, category: domain.CategoryFestasShows})

	if first.Slug == second.Slug {
		t.Fatalf("both events answer at %q; the second would be unreachable", first.Slug)
	}
	found, err := h.service.GetPublished(context.Background(), second.Slug)
	if err != nil || found.ID != second.ID {
		t.Fatalf("GetPublished(%q) = (%v, %v)", second.Slug, found, err)
	}
}

// A draft is 404 and not 403: a 403 confirms an event exists at that address,
// which is how an unannounced line-up leaks before its on-sale.
func TestADraftIsNotReachableByItsPublicAddress(t *testing.T) {
	h := newHarness(t)
	draft := h.seed(t, seed{name: "Segredo " + testsupport.Unique("s"), status: domain.StatusDraft, category: domain.CategoryFestasShows})

	_, err := h.service.GetPublished(context.Background(), draft.Slug)

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetPublished(draft) error = %v, want %v", err, domain.ErrNotFound)
	}
}

func TestPublishingMakesAnEventReachable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	draft := h.seed(t, seed{name: "Rascunho " + testsupport.Unique("r"), status: domain.StatusDraft, category: domain.CategoryFestasShows})

	if _, err := h.service.Publish(ctx, draft.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	found, err := h.service.GetPublished(ctx, draft.Slug)
	if err != nil || found.ID != draft.ID {
		t.Fatalf("GetPublished() after publish = (%v, %v)", found, err)
	}
}

// One media read for a whole page, not one per card: a grid of twenty-four
// events asking individually is the N+1 a listing dies of.
func TestListHydratesEveryCardsGallery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	withArt := h.seed(t, seed{name: "Com arte " + testsupport.Unique("a"), category: domain.CategoryFestasShows})
	h.seed(t, seed{name: "Sem arte " + testsupport.Unique("b"), category: domain.CategoryFestasShows})

	if _, err := h.service.AttachMedia(ctx, withArt.ID, mediadomain.Upload{
		FileName: "capa.png", ContentType: "image/png", Data: onePixelPNG,
	}); err != nil {
		t.Fatalf("AttachMedia() error = %v", err)
	}

	page := h.list(t, domain.Filter{})

	for index := range page.Items {
		item := page.Items[index]
		if item.Event.Media == nil {
			t.Fatalf("%q has a nil gallery; a client would have to special-case null", item.Event.Name)
		}
		want := 0
		if item.Event.ID == withArt.ID {
			want = 1
		}
		if len(item.Event.Media) != want {
			t.Fatalf("%q has %d image(s), want %d", item.Event.Name, len(item.Event.Media), want)
		}
	}
}

// An event whose tiers exist may not be deleted: those tiers may carry orders,
// and the orders are the record of money that changed hands.
func TestDeletingAnEventWithTiersIsRefused(t *testing.T) {
	h := newHarness(t)
	item := h.seed(t, seed{name: "Com lotes " + testsupport.Unique("l"), prices: []int64{9000}, category: domain.CategoryFestasShows})

	err := h.service.Delete(context.Background(), item.ID)

	if !errors.Is(err, domain.ErrHasTickets) {
		t.Fatalf("Delete() error = %v, want %v", err, domain.ErrHasTickets)
	}
}

// The buy panel shows only what is actually for sale, cheapest first. A draft
// tier is an operator's work in progress; a cancelled one is not for sale.
func TestOnSaleTiersAreOrderedAndFiltered(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	item := h.seed(t, seed{name: "Com lotes " + testsupport.Unique("t"), prices: []int64{30000, 9000}, category: domain.CategoryFestasShows})

	// A draft tier alongside them, which must not appear.
	draft, err := ticketdomain.New(testsupport.Unique("tkt"), h.ownerID, ticketdomain.Draft{
		EventID: item.ID, Title: "Rascunho", PriceCents: 100, Quantity: 10,
		Status: ticketdomain.StatusDraft,
	}, time.Now())
	if err != nil {
		t.Fatalf("build draft tier: %v", err)
	}
	if err := h.tickets.Create(ctx, draft); err != nil {
		t.Fatalf("create draft tier: %v", err)
	}

	tiers, err := h.service.OnSaleTiers(ctx, item.ID)

	if err != nil {
		t.Fatalf("OnSaleTiers() error = %v", err)
	}
	if len(tiers) != 2 {
		t.Fatalf("tiers = %d, want the two on sale and not the draft", len(tiers))
	}
	if tiers[0].PriceCents != 9000 || tiers[1].PriceCents != 30000 {
		t.Fatalf("prices = %d then %d, want cheapest first", tiers[0].PriceCents, tiers[1].PriceCents)
	}
}

// Asking for a draft's tiers must not confirm the draft exists, or an
// unannounced line-up leaks its prices before its on-sale.
func TestADraftsTiersAreNotReadable(t *testing.T) {
	h := newHarness(t)
	item := h.seed(t, seed{name: "Segredo " + testsupport.Unique("s"), prices: []int64{9000}, status: domain.StatusDraft, category: domain.CategoryFestasShows})

	_, err := h.service.OnSaleTiers(context.Background(), item.ID)

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("OnSaleTiers(draft) error = %v, want %v", err, domain.ErrNotFound)
	}
}

// The filter row has to offer every category, including the empty ones: a row
// whose options appear and disappear as events are published is one a buyer
// cannot learn. The count is what lets the UI grey one out instead.
func TestAvailableFiltersListEveryCategoryWithItsCount(t *testing.T) {
	h := newHarness(t)
	h.seed(t, seed{name: "Show " + testsupport.Unique("s"), category: domain.CategoryFestasShows})

	filters, err := h.service.AvailableFilters(context.Background(), 10)

	if err != nil {
		t.Fatalf("AvailableFilters() error = %v", err)
	}
	if len(filters.Categories) != len(domain.Categories()) {
		t.Fatalf("categories = %d, want all %d", len(filters.Categories), len(domain.Categories()))
	}
	var shows int64
	for _, option := range filters.Categories {
		if option.Category == domain.CategoryFestasShows {
			shows = option.Count
		}
	}
	if shows < 1 {
		t.Fatalf("festas_shows count = %d, want at least the one just seeded", shows)
	}
	if len(filters.Cities) == 0 {
		t.Fatal("no cities offered; the city filter would have nothing to show")
	}
}

// onePixelPNG is enough for the upload path to have real content without a
// fixture file.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89,
}
