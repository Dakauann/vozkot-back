// Package event is the application rules for the thing a buyer browses.
//
// Two of them are worth stating up front, because they are the reason this is a
// use case and not a repository call:
//
//   - A public listing and an operator's listing are the same query with
//     different permissions. Rather than two code paths that will drift, there
//     is one, and the caller says which it is by supplying a viewer.
//   - Geocoding is best-effort and never blocks a save. An address no service
//     recognises is still a real address, and an event nobody can publish
//     because a third party was down is a worse product than an event with no
//     map.
package event

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"time"

	"fmt"

	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/event"
	mediadomain "vozkot/domain/media"
	ticketdomain "vozkot/domain/ticket"
)

type Service struct {
	repository domain.Repository
	// tiers reads the prices an event sells at. The catalogue owns the
	// happening; the tiers own the money, and the event page needs both.
	tiers ticketdomain.Repository
	media mediadomain.Library
	// geocoder is optional. Without one, coordinates are whatever an operator
	// placed by hand, and the rest of the product is unchanged.
	geocoder domain.Geocoder
	now      func() time.Time
	newID    func() string
}

func NewService(
	repository domain.Repository,
	tiers ticketdomain.Repository,
	media mediadomain.Library,
	geocoder domain.Geocoder,
) *Service {
	return &Service{
		repository: repository,
		tiers:      tiers,
		media:      media,
		geocoder:   geocoder,
		now:        time.Now,
		newID:      randomID,
	}
}

// CreateInput is one new event.
type CreateInput struct {
	OwnerID     string
	Name        string
	Description string
	Category    domain.Category
	Location    domain.Location
	StartsAt    time.Time
	EndsAt      *time.Time
	Status      domain.Status
	// SalesMode is how the event sells. Empty defaults to counted on create.
	SalesMode domain.SalesMode
}

// UpdateInput is an edit. Owner is not among the fields: an event does not
// change hands.
type UpdateInput struct {
	Name        string
	Description string
	Category    domain.Category
	Location    domain.Location
	StartsAt    time.Time
	EndsAt      *time.Time
	Status      domain.Status
	// SalesMode is how the event sells. Empty leaves it as it was.
	SalesMode domain.SalesMode
}

func (s *Service) Create(ctx context.Context, input CreateInput) (*domain.Event, error) {
	item, err := domain.New(s.newID(), input.OwnerID, domain.Draft{
		Name:        input.Name,
		Description: input.Description,
		Category:    input.Category,
		Location:    input.Location,
		StartsAt:    input.StartsAt,
		EndsAt:      input.EndsAt,
		Status:      input.Status,
		SalesMode:   input.SalesMode,
	}, s.now())
	if err != nil {
		return nil, err
	}

	// The slug is settled before the write rather than after a failure: two
	// operators selling "Festival Aurora" is ordinary, and the second one must
	// still get a working page.
	slug, err := s.availableSlug(ctx, item.Slug, item.ID)
	if err != nil {
		return nil, err
	}
	item.Slug = slug

	s.locate(ctx, item, nil)

	if err := s.repository.Create(ctx, item); err != nil {
		return nil, err
	}
	item.Media = []mediadomain.Media{}
	return item, nil
}

func (s *Service) Update(
	ctx context.Context,
	actor authdomain.Actor,
	id string,
	input UpdateInput,
) (*domain.Event, error) {
	item, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	previous := item.Location

	if err := item.Apply(domain.Draft{
		Name:        input.Name,
		Description: input.Description,
		Category:    input.Category,
		Location:    input.Location,
		StartsAt:    input.StartsAt,
		EndsAt:      input.EndsAt,
		Status:      input.Status,
		SalesMode:   input.SalesMode,
	}, s.now()); err != nil {
		return nil, err
	}

	s.locate(ctx, item, &previous)

	if err := s.repository.Update(ctx, item); err != nil {
		return nil, err
	}
	return s.get(ctx, id)
}

// locate fills in coordinates, and knows when not to.
//
// Three rules, in order, and each one exists because of a way this goes wrong:
//
//  1. Coordinates the caller supplied are kept as they are. That is an operator
//     who dragged the pin, and a person looking at a satellite image beats any
//     geocoder, so a later edit to the description must not quietly move the
//     venue back to where a postcode said it was.
//  2. An address that did not change is not looked up again. Editing a title
//     should not spend a third-party call, and on a provider with a rate limit
//     it would eventually cost the save itself.
//  3. A geocoder that fails costs the map and never the event.
func (s *Service) locate(ctx context.Context, item *domain.Event, previous *domain.Location) {
	if s.geocoder == nil || item.Location.HasCoordinates() {
		return
	}
	if previous != nil && previous.HasCoordinates() && sameAddress(*previous, item.Location) {
		// The address is unchanged and it already had a point; keep it.
		item.Location.Latitude = previous.Latitude
		item.Location.Longitude = previous.Longitude
		return
	}

	found, err := s.geocoder.Locate(ctx, item.Location)
	if err != nil {
		// Logged, not returned. The alternative is an operator who cannot
		// publish tonight's show because a geocoding API is having an evening.
		log.Printf("event: geocoding %q failed, saving without coordinates: %v", item.Location.City, err)
		return
	}
	if found == nil {
		return
	}
	item.Location.Latitude = &found.Latitude
	item.Location.Longitude = &found.Longitude
}

// sameAddress compares the parts a geocoder actually reads.
func sameAddress(a, b domain.Location) bool {
	return strings.EqualFold(strings.TrimSpace(a.Address), strings.TrimSpace(b.Address)) &&
		strings.EqualFold(strings.TrimSpace(a.City), strings.TrimSpace(b.City)) &&
		strings.EqualFold(strings.TrimSpace(a.UF), strings.TrimSpace(b.UF)) &&
		strings.EqualFold(strings.TrimSpace(a.PostalCode), strings.TrimSpace(b.PostalCode))
}

// availableSlug finds an address no other event is using.
func (s *Service) availableSlug(ctx context.Context, base, eventID string) (string, error) {
	candidate := base
	for attempt := 0; attempt < 4; attempt++ {
		taken, err := s.repository.SlugExists(ctx, candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		// The id's tail rather than a counter: a counter has to be read back
		// under a lock to be correct, and a random tail does not.
		suffix := strings.TrimPrefix(eventID, "evt_")
		if len(suffix) > 6 {
			suffix = suffix[:6]
		}
		if attempt > 0 {
			suffix = randomHex(4)
		}
		candidate = base + "-" + suffix
	}
	return base + "-" + randomHex(6), nil
}

// owned loads an event and refuses one belonging to somebody else.
//
// The single authorisation point of this package. Every operator method below
// starts here, which is what keeps a new one from being the method that
// forgot; it used to be a helper in the HTTP handler, where eight routes each
// remembered to call it and nothing made them.
func (s *Service) owned(ctx context.Context, actor authdomain.Actor, id string) (*domain.Event, error) {
	item, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !actor.MayReach(item.OwnerID) {
		return nil, fmt.Errorf("%w: this event belongs to another operator", authdomain.ErrForbidden)
	}
	return item, nil
}

// Get returns one event to its owner.
func (s *Service) Get(ctx context.Context, actor authdomain.Actor, id string) (*domain.Event, error) {
	return s.owned(ctx, actor, id)
}

func (s *Service) get(ctx context.Context, id string) (*domain.Event, error) {
	item, err := s.repository.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.withMedia(ctx, item)
}

// GetPublished resolves the public page's URL, and refuses a draft.
//
// A draft is answered as NOT FOUND rather than as forbidden, deliberately: a
// 403 confirms that an event exists at that address, which is how an
// unannounced line-up leaks before its on-sale.
func (s *Service) GetPublished(ctx context.Context, slug string) (*domain.Event, error) {
	item, err := s.repository.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if item.Status == domain.StatusDraft {
		return nil, domain.ErrNotFound
	}
	return s.withMedia(ctx, item)
}

func (s *Service) withMedia(ctx context.Context, item *domain.Event) (*domain.Event, error) {
	gallery, err := s.media.ListByEvent(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	if gallery == nil {
		// Never nil: a client that has to special-case null for "no images" is
		// a client that will forget to.
		gallery = []mediadomain.Media{}
	}
	item.Media = gallery
	return item, nil
}

// List is the catalogue query, for buyers and operators alike.
// List is the operator's own events, drafts included.
//
// Scoped here rather than by the caller: an unscoped listing is every box
// office's unannounced line-ups, and the transport layer used to be the only
// thing standing between them.
func (s *Service) List(ctx context.Context, actor authdomain.Actor, filter domain.Filter) (domain.Page, error) {
	if !actor.IsAdmin() {
		if !actor.Authenticated() {
			return domain.Page{}, authdomain.ErrUnauthorized
		}
		filter.OwnerID = actor.ID
	}
	return s.list(ctx, filter)
}

// ListPublic is the catalogue: published events only, owned by anybody.
//
// The status is pinned here and the owner cleared, so no caller can widen it.
func (s *Service) ListPublic(ctx context.Context, filter domain.Filter) (domain.Page, error) {
	filter.Status = domain.StatusPublished
	filter.OwnerID = ""
	return s.list(ctx, filter)
}

// ByIDs resolves events the caller already holds ids for.
//
// It exists for hydrating a name onto something else — a buyer's own order
// list — and is deliberately NOT ownership-scoped: a buyer does not own the
// event they bought a ticket to. It takes ids rather than a filter so it can
// never be turned into a general listing of somebody else's catalogue.
func (s *Service) ByIDs(ctx context.Context, ids []string) (domain.Page, error) {
	if len(ids) == 0 {
		return domain.Page{}, nil
	}
	return s.list(ctx, domain.Filter{IDs: ids, Limit: len(ids)}.Normalize())
}

// list is the query itself, with the gallery hydration every listing needs.
// Unexported: every caller above has already said which audience it serves.
func (s *Service) list(ctx context.Context, filter domain.Filter) (domain.Page, error) {
	page, err := s.repository.List(ctx, filter)
	if err != nil {
		return domain.Page{}, err
	}
	if len(page.Items) == 0 {
		return page, nil
	}

	// One media read for the whole page rather than one per card: a grid of
	// twenty-four events asking individually is the N+1 a listing dies of.
	ids := make([]string, 0, len(page.Items))
	for index := range page.Items {
		ids = append(ids, page.Items[index].Event.ID)
	}
	galleries, err := s.media.ListByEvents(ctx, ids)
	if err != nil {
		return domain.Page{}, err
	}
	for index := range page.Items {
		gallery := galleries[page.Items[index].Event.ID]
		if gallery == nil {
			gallery = []mediadomain.Media{}
		}
		page.Items[index].Event.Media = gallery
	}
	return page, nil
}

// Publish and Unpublish are their own operations rather than an Update with a
// status field, because that is how an operator thinks about them and because
// the listing's visibility should not be changed by accident while editing a
// description.
func (s *Service) Publish(ctx context.Context, actor authdomain.Actor, id string) (*domain.Event, error) {
	return s.changeStatus(ctx, actor, id, domain.StatusPublished)
}

func (s *Service) Unpublish(ctx context.Context, actor authdomain.Actor, id string) (*domain.Event, error) {
	return s.changeStatus(ctx, actor, id, domain.StatusDraft)
}

func (s *Service) Cancel(ctx context.Context, actor authdomain.Actor, id string) (*domain.Event, error) {
	return s.changeStatus(ctx, actor, id, domain.StatusCancelled)
}

func (s *Service) changeStatus(
	ctx context.Context,
	actor authdomain.Actor,
	id string,
	status domain.Status,
) (*domain.Event, error) {
	item, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if !status.Valid() {
		return nil, domain.ErrInvalidStatus
	}
	item.Status = status
	item.UpdatedAt = s.now().UTC()
	if err := s.repository.Update(ctx, item); err != nil {
		return nil, err
	}
	return s.get(ctx, id)
}

// PlacePin records coordinates a person chose.
//
// Separate from Update because it means something different: these are trusted
// above anything a geocoder produces, and nothing later overwrites them.
func (s *Service) PlacePin(
	ctx context.Context,
	actor authdomain.Actor,
	id string,
	latitude, longitude float64,
) (*domain.Event, error) {
	item, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	location := item.Location
	location.Latitude = &latitude
	location.Longitude = &longitude
	if err := item.Apply(domain.Draft{
		Name:        item.Name,
		Description: item.Description,
		Category:    item.Category,
		Location:    location,
		StartsAt:    item.StartsAt,
		EndsAt:      item.EndsAt,
		Status:      item.Status,
		SalesMode:   item.SalesMode,
	}, s.now()); err != nil {
		return nil, err
	}
	if err := s.repository.Update(ctx, item); err != nil {
		return nil, err
	}
	return s.get(ctx, id)
}

// OnSaleTiers is what an event page's buy panel is built from.
//
// It refuses an event a buyer cannot reach, for the same reason GetPublished
// does: answering with the tiers of a draft would confirm the draft exists and
// leak an unannounced line-up's prices along with it.
//
// Only tiers actually on sale. A draft tier is an operator's work in progress
// and a cancelled one is not for sale; neither belongs on a page whose job is
// to be bought from.
func (s *Service) OnSaleTiers(ctx context.Context, eventID string) ([]ticketdomain.Ticket, error) {
	item, err := s.repository.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if !item.Status.Visible() {
		return nil, domain.ErrNotFound
	}
	return s.tiers.List(ctx, ticketdomain.Filter{
		EventID: item.ID,
		Status:  ticketdomain.StatusOnSale,
		Sort:    ticketdomain.SortPrice,
	})
}

// AttachMedia stores one image or clip against an existing event.
//
// The event is loaded first so an upload can never invent the listing it
// belongs to, which is what stops a caller writing objects into a bucket under
// an id nothing will ever point at.
func (s *Service) AttachMedia(
	ctx context.Context,
	actor authdomain.Actor,
	eventID string,
	upload mediadomain.Upload,
) (*mediadomain.Media, error) {
	item, err := s.owned(ctx, actor, eventID)
	if err != nil {
		return nil, err
	}
	upload.EventID = item.ID
	return s.media.Add(ctx, upload)
}

func (s *Service) RemoveMedia(ctx context.Context, actor authdomain.Actor, eventID, mediaID string) error {
	if _, err := s.owned(ctx, actor, eventID); err != nil {
		return err
	}
	return s.media.Remove(ctx, eventID, mediaID)
}

// Delete removes the gallery before the event so that a failure at the storage
// step still leaves an event the operator can retry on, rather than an
// unreachable row and a bucket full of assets nothing points to.
func (s *Service) Delete(ctx context.Context, actor authdomain.Actor, id string) error {
	if _, err := s.owned(ctx, actor, id); err != nil {
		return err
	}
	if err := s.media.RemoveAllByEvent(ctx, id); err != nil && !errors.Is(err, mediadomain.ErrNotFound) {
		return err
	}
	return s.repository.Delete(ctx, id)
}

// Filters is what the listing page needs to render its own controls: the
// categories that have something in them, and the cities that do.
type Filters struct {
	Categories []CategoryOption
	Cities     []domain.CityCount
}

type CategoryOption struct {
	Category domain.Category
	Count    int64
}

// AvailableFilters lists every category in taxonomy order with its count, and
// the busiest cities.
//
// Every category, including the empty ones, because a filter row whose options
// appear and disappear as events are published is one a buyer cannot learn. The
// count is what lets the UI grey out an empty one instead.
func (s *Service) AvailableFilters(ctx context.Context, cityLimit int) (Filters, error) {
	counts, err := s.repository.CategoryCounts(ctx)
	if err != nil {
		return Filters{}, err
	}
	options := make([]CategoryOption, 0, len(domain.Categories()))
	for _, category := range domain.Categories() {
		options = append(options, CategoryOption{Category: category, Count: counts[category]})
	}

	cities, err := s.repository.Cities(ctx, cityLimit)
	if err != nil {
		return Filters{}, err
	}
	return Filters{Categories: options, Cities: cities}, nil
}

func randomID() string { return "evt_" + randomHex(8) }

func randomHex(bytes int) string {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return time.Now().UTC().Format("150405.000000")
	}
	return hex.EncodeToString(buffer)
}
