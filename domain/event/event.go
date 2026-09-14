// Package event is what a buyer browses: one happening, at one place, on one
// date, with one or more ticket tiers underneath it.
//
// It exists because an event and a ticket are not the same thing, and the
// difference is the whole shape of the product. A ticket here is an INGRESSO;
// Pista, Camarote, Meia-entrada, and an event that sells three of them is one
// listing, one page, one map pin and one category, not three. Before this
// package the two were the same row and an event was a repeated string in an
// `event_name` column, which meant a listing had to group by that string, two
// tiers of the same night could disagree about where it was, and there was
// nowhere to put a category or a coordinate that belonged to the event rather
// than to one of its prices.
//
// So: events own everything about the happening. Tickets own price and stock
// and point at their event. Orders keep pointing at tickets, because money is
// paid for a tier and not for an event, and none of the inventory machinery
// that guards it had to move.
package event

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"vozkot/domain/media"
)

// Status is where an event stands in its own life, independent of whether any
// particular tier still has stock.
type Status string

const (
	// StatusDraft is being written. It is the only state invisible to buyers,
	// and the only one a new event starts in.
	StatusDraft Status = "draft"
	// StatusPublished is listed and reachable. Whether a buyer can actually buy
	// depends on the tiers underneath, which have their own status.
	StatusPublished Status = "published"
	// StatusCancelled called the event off. The page stays reachable, people
	// who bought need to find it, and nothing new may be sold.
	StatusCancelled Status = "cancelled"
)

func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusPublished, StatusCancelled:
		return true
	default:
		return false
	}
}

// Visible reports whether an event belongs in a public listing.
func (s Status) Visible() bool { return s == StatusPublished }

// Category is the fixed taxonomy a buyer browses by.
//
// Fixed, and not free text an operator types, for two reasons that both show up
// immediately in the product: a filter over free text is a filter that lists
// "Show", "show", "Shows" and "Música ao vivo" as four categories, and a
// listing page has to render an icon and a colour per category, which it cannot
// do for a value it has never seen.
//
// The set mirrors the taxonomy Brazilian buyers already know from Sympla, in
// their language, because a category is a word a buyer recognises rather than a
// word a developer invents.
//
// What is deliberately NOT here matters as much as what is. Sympla's navigation
// also offers "Grátis" and "Online" beside the real categories, and both are
// filters wearing category clothing: an event is not free INSTEAD of being a
// show, it is a free show. They are Filter.OnlyFree and an event format, so
// that a buyer can ask for a free show rather than having to choose.
type Category string

const (
	CategoryFestasShows     Category = "festas_shows"
	CategoryTeatrosEspetacs Category = "teatros_espetaculos"
	CategoryStandUp         Category = "stand_up_comedy"
	CategoryCursosWorkshops Category = "cursos_workshops"
	CategoryCongressos      Category = "congressos_palestras"
	CategoryEsportivo       Category = "esportivo"
	CategoryGastronomia     Category = "gastronomia"
	CategoryReligiao        Category = "religiao_espiritualidade"
	CategoryPasseiosTours   Category = "passeios_tours"
	CategoryInfantil        Category = "infantil"
	CategoryGamesGeek       Category = "games_geek"
	CategoryModaBeleza      Category = "moda_beleza"
	CategorySaudeBemEstar   Category = "saude_bem_estar"
	CategoryArteCultura     Category = "arte_cultura_lazer"
	CategoryPride           Category = "pride"
	// CategoryOutros is the honest escape hatch. Without one, an operator whose
	// event fits nothing above picks the nearest wrong answer, and the filter
	// that was supposed to help a buyer starts lying to them.
	CategoryOutros Category = "outros"
)

// Categories is the whole taxonomy, in the order a listing should offer it:
// the ones that carry the most events first, with the escape hatch last.
func Categories() []Category {
	return []Category{
		CategoryFestasShows,
		CategoryTeatrosEspetacs,
		CategoryStandUp,
		CategoryCursosWorkshops,
		CategoryCongressos,
		CategoryEsportivo,
		CategoryGastronomia,
		CategoryReligiao,
		CategoryPasseiosTours,
		CategoryInfantil,
		CategoryGamesGeek,
		CategoryModaBeleza,
		CategorySaudeBemEstar,
		CategoryArteCultura,
		CategoryPride,
		CategoryOutros,
	}
}

func (c Category) Valid() bool {
	for _, known := range Categories() {
		if c == known {
			return true
		}
	}
	return false
}

var (
	ErrNotFound         = errors.New("event not found")
	ErrInvalidName      = errors.New("event name is required")
	ErrInvalidCategory  = errors.New("event category is not one of the supported categories")
	ErrInvalidVenue     = errors.New("venue is required")
	ErrInvalidCity      = errors.New("city is required")
	ErrInvalidStartsAt  = errors.New("event start is required")
	ErrEndsBeforeStarts = errors.New("event cannot end before it starts")
	ErrInvalidStatus    = errors.New("event status is invalid")
	ErrInvalidLocation  = errors.New("latitude and longitude are outside the valid range")
	// ErrHasTickets refuses to delete an event that still has tiers. The tiers
	// may have orders against them, and those orders are the record of money
	// that changed hands.
	ErrHasTickets = errors.New("event has ticket tiers and cannot be deleted")
	ErrSlugTaken  = errors.New("another event already uses this address")
)

// Location is where the event physically happens.
//
// The address is what a person reads and the coordinates are what a map draws,
// and they are stored together because they are answers to the same question
// that must not drift apart. Coordinates are OPTIONAL: an event whose address
// no geocoder recognises is still a real event, and the page simply shows the
// address without a map rather than refusing to exist.
type Location struct {
	// Venue is the name a buyer would say out loud: "Arena Castelão".
	Venue string
	// Address is the street line, as written.
	Address string
	// Neighborhood, City and UF are the rest of a Brazilian address. City is
	// what the listing filters by, so it is required where the others are not.
	Neighborhood string
	City         string
	UF           string
	PostalCode   string
	// Latitude and Longitude are nil together or set together. A half-set pair
	// would put a pin in the Atlantic.
	Latitude  *float64
	Longitude *float64
}

// HasCoordinates reports whether a map can be drawn for this location.
func (l Location) HasCoordinates() bool { return l.Latitude != nil && l.Longitude != nil }

func (l Location) validate() error {
	if strings.TrimSpace(l.Venue) == "" {
		return ErrInvalidVenue
	}
	if strings.TrimSpace(l.City) == "" {
		return ErrInvalidCity
	}
	if (l.Latitude == nil) != (l.Longitude == nil) {
		return ErrInvalidLocation
	}
	if l.Latitude != nil {
		if *l.Latitude < -90 || *l.Latitude > 90 || *l.Longitude < -180 || *l.Longitude > 180 {
			return ErrInvalidLocation
		}
	}
	return nil
}

// Event is one happening a buyer can find, read about, and buy into.
//
// No json tags: the wire format is the delivery layer's decision, and the two
// drift apart the moment the API needs a field the domain does not have.
type Event struct {
	ID      string
	OwnerID string
	// Slug is the human-readable half of the public URL. It is derived from the
	// name once, at creation, and then never changes on its own, a link that
	// someone shared has to keep working after the name is corrected.
	Slug        string
	Name        string
	Description string
	Category    Category
	Location    Location

	StartsAt time.Time
	// EndsAt is optional. A single-night show has no meaningful end, while a
	// three-day festival does, and a listing renders the two differently.
	EndsAt *time.Time

	Status Status
	Media  []media.Media

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Draft is what an operator supplies. Create and update take the same shape
// because they validate the same rules.
type Draft struct {
	Name        string
	Description string
	Category    Category
	Location    Location
	StartsAt    time.Time
	EndsAt      *time.Time
	Status      Status
}

// New builds an event. It starts as a draft unless the caller asks otherwise,
// so nothing reaches buyers by accident.
func New(id, ownerID string, draft Draft, now time.Time) (*Event, error) {
	normalized, err := draft.normalize()
	if err != nil {
		return nil, err
	}
	if normalized.Status == "" {
		normalized.Status = StatusDraft
	}
	if !normalized.Status.Valid() {
		return nil, ErrInvalidStatus
	}

	timestamp := now.UTC()
	return &Event{
		ID:          id,
		OwnerID:     ownerID,
		Slug:        Slugify(normalized.Name),
		Name:        normalized.Name,
		Description: normalized.Description,
		Category:    normalized.Category,
		Location:    normalized.Location,
		StartsAt:    normalized.StartsAt,
		EndsAt:      normalized.EndsAt,
		Status:      normalized.Status,
		CreatedAt:   timestamp,
		UpdatedAt:   timestamp,
	}, nil
}

// Apply updates the operator-editable fields.
//
// The slug is deliberately NOT recomputed. A corrected typo in a name must not
// break every link already shared, and a listing that silently changes its own
// URLs is a listing whose search results 404.
func (e *Event) Apply(draft Draft, now time.Time) error {
	normalized, err := draft.normalize()
	if err != nil {
		return err
	}
	if normalized.Status == "" {
		normalized.Status = e.Status
	}
	if !normalized.Status.Valid() {
		return ErrInvalidStatus
	}

	e.Name = normalized.Name
	e.Description = normalized.Description
	e.Category = normalized.Category
	e.Location = normalized.Location
	e.StartsAt = normalized.StartsAt
	e.EndsAt = normalized.EndsAt
	e.Status = normalized.Status
	e.UpdatedAt = now.UTC()
	return nil
}

func (d Draft) normalize() (Draft, error) {
	d.Name = strings.TrimSpace(d.Name)
	d.Description = strings.TrimSpace(d.Description)
	d.Location.Venue = strings.TrimSpace(d.Location.Venue)
	d.Location.Address = strings.TrimSpace(d.Location.Address)
	d.Location.Neighborhood = strings.TrimSpace(d.Location.Neighborhood)
	d.Location.City = strings.TrimSpace(d.Location.City)
	d.Location.UF = strings.ToUpper(strings.TrimSpace(d.Location.UF))
	d.Location.PostalCode = strings.TrimSpace(d.Location.PostalCode)

	if d.Name == "" {
		return d, ErrInvalidName
	}
	if d.Category == "" {
		d.Category = CategoryOutros
	}
	if !d.Category.Valid() {
		return d, ErrInvalidCategory
	}
	if err := d.Location.validate(); err != nil {
		return d, err
	}
	if d.StartsAt.IsZero() {
		return d, ErrInvalidStartsAt
	}
	if d.EndsAt != nil && d.EndsAt.Before(d.StartsAt) {
		return d, ErrEndsBeforeStarts
	}
	return d, nil
}

// Slugify turns a name into the URL-safe half of a public address.
//
// Accents are folded rather than dropped, so "Sertão" becomes "sertao" and not
// "serto"; a Brazilian catalogue where every other name loses a letter is a
// catalogue nobody can guess a URL in.
func Slugify(name string) string {
	var builder strings.Builder
	previousDash := true // leading dashes are suppressed

	for _, char := range strings.ToLower(strings.TrimSpace(name)) {
		folded, ok := fold(char)
		switch {
		case ok:
			builder.WriteRune(folded)
			previousDash = false
		case unicode.IsLetter(char) || unicode.IsDigit(char):
			builder.WriteRune(char)
			previousDash = false
		case !previousDash:
			builder.WriteRune('-')
			previousDash = true
		}
	}

	slug := strings.Trim(builder.String(), "-")
	if len(slug) > 80 {
		slug = strings.Trim(slug[:80], "-")
	}
	if slug == "" {
		// A name written entirely in a script this fold does not cover still
		// needs an address. The id is appended by the caller, so "evento" alone
		// is never ambiguous.
		return "evento"
	}
	return slug
}

// fold maps the accented letters Portuguese actually uses onto their base.
var folding = map[rune]rune{
	'á': 'a', 'à': 'a', 'ã': 'a', 'â': 'a', 'ä': 'a',
	'é': 'e', 'ê': 'e', 'è': 'e', 'ë': 'e',
	'í': 'i', 'î': 'i', 'ì': 'i', 'ï': 'i',
	'ó': 'o', 'ô': 'o', 'õ': 'o', 'ò': 'o', 'ö': 'o',
	'ú': 'u', 'û': 'u', 'ù': 'u', 'ü': 'u',
	'ç': 'c', 'ñ': 'n',
}

func fold(char rune) (rune, bool) {
	folded, ok := folding[char]
	return folded, ok
}
