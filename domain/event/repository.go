package event

import (
	"context"
	"time"
)

// Sort names the orders a listing offers.
type Sort string

const (
	// SortRelevance ranks by how well an event matches the search terms. It is
	// meaningless without a query, so a listing with no terms falls back to
	// SortStartsAt rather than returning rows in whatever order the index
	// happened to produce.
	SortRelevance Sort = "relevance"
	// SortStartsAt is the default a buyer expects: what is happening soonest.
	SortStartsAt Sort = "starts_at"
	// SortPrice orders by the cheapest tier an event still sells.
	SortPrice Sort = "price"
	// SortNewest is what an operator wants when checking their own work.
	SortNewest Sort = "created_at"
)

func (s Sort) Valid() bool {
	switch s {
	case SortRelevance, SortStartsAt, SortPrice, SortNewest:
		return true
	default:
		return false
	}
}

// MaxPageSize caps how much one request may ask for. Unbounded page sizes are
// how a public listing becomes a way to download the whole catalogue in one
// request, and how one slow client holds a database connection for a second.
const MaxPageSize = 60

// DefaultPageSize fills a listing grid without a scroll to nowhere.
const DefaultPageSize = 24

// MaxOffset bounds how deep a caller may page.
//
// Offset pagination re-walks every row it skips, so page 5,000 costs five
// thousand pages of work to return one. A catalogue is browsed, not
// paginated to its end: real buyers refine the filter instead. The cap turns
// an expensive query into an honest refusal, and keeps a crawler from
// discovering that page 200,000 is a denial of service.
const MaxOffset = 5_000

// Filter is the query a listing is built from. The zero value lists every
// published event, soonest first.
type Filter struct {
	// Query is what the buyer typed. It is matched against the event's name,
	// description, venue and city.
	Query string
	// Category, when set, restricts to one category.
	Category Category
	// City matches the event's city exactly, folded for case and accents, which
	// is what makes "sao paulo" find "São Paulo".
	City string
	// StartsFrom and StartsUntil bound the event date. Both are optional and
	// either may be given alone.
	StartsFrom  *time.Time
	StartsUntil *time.Time
	// MaxPriceCents, when set, keeps events whose CHEAPEST available tier costs
	// no more than this. The cheapest tier is the right one to compare: a buyer
	// filtering by price is asking what they can get in, not what the best seat
	// costs.
	MaxPriceCents *int64
	// OnlyFree keeps events whose cheapest tier is zero.
	OnlyFree bool
	// Status restricts by event status. A PUBLIC listing must always set this
	// to StatusPublished; leaving it empty is what an operator's own listing
	// does, and it shows drafts.
	Status Status
	// OwnerID scopes to one operator's catalogue.
	OwnerID string
	// IDs restricts to a known set, for resolving the events a page of
	// something else refers to. An orders listing needs the show, the night and
	// the venue behind twenty orders; asking for them one at a time is the N+1
	// this field exists to avoid. Empty means no restriction, so a caller that
	// computed an empty set must not call at all.
	IDs []string
	// AvailableOnly drops events whose every tier is sold out or off sale. It
	// is what a buyer means by "show me what I can buy".
	AvailableOnly bool

	Sort   Sort
	Limit  int
	Offset int
}

// Normalize clamps a filter into the range the repository will honour, so every
// caller gets the same bounds whether it remembered to apply them or not.
func (f Filter) Normalize() Filter {
	if f.Limit <= 0 {
		f.Limit = DefaultPageSize
	}
	if f.Limit > MaxPageSize {
		f.Limit = MaxPageSize
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	if f.Offset > MaxOffset {
		f.Offset = MaxOffset
	}
	if !f.Sort.Valid() {
		f.Sort = SortStartsAt
	}
	// Relevance without terms is not an order, it is a coin toss.
	if f.Sort == SortRelevance && f.Query == "" {
		f.Sort = SortStartsAt
	}
	return f
}

// Listing is one event as a listing row: the event itself plus the two numbers
// a card shows that do not live on the event.
//
// They are returned together rather than fetched per row because a grid of
// twenty-four cards asking for its own price is twenty-four extra queries, and
// that is the classic N+1 a catalogue page dies of.
type Listing struct {
	Event Event
	// FromPriceCents is the cheapest tier currently on sale. Nil when the event
	// has no tier a buyer could take, which is what "Esgotado" renders from.
	FromPriceCents *int64
	// AvailableTickets is how many tickets remain across every tier on sale.
	AvailableTickets int
}

// Page is a listing plus the total it was taken from.
type Page struct {
	Items []Listing
	Total int64
}

// Repository persists events.
type Repository interface {
	Create(ctx context.Context, item *Event) error
	GetByID(ctx context.Context, id string) (*Event, error)
	// GetBySlug is what the public page resolves a URL with.
	GetBySlug(ctx context.Context, slug string) (*Event, error)
	// List returns a page of listings and the total matching the same filter.
	//
	// One call rather than a List plus a Count, because the two are one
	// question and answering it twice is how a listing's count disagrees with
	// its rows when something is published between the queries.
	List(ctx context.Context, filter Filter) (Page, error)
	Update(ctx context.Context, item *Event) error
	Delete(ctx context.Context, id string) error
	// SlugExists reports whether a slug is already taken, so creation can
	// disambiguate before it writes rather than after it fails.
	SlugExists(ctx context.Context, slug string) (bool, error)
	// Cities lists the cities that currently have published events, for the
	// filter's own options. A filter offering a city with nothing in it is a
	// filter that leads every buyer who picks it to an empty page.
	Cities(ctx context.Context, limit int) ([]CityCount, error)
	// CategoryCounts reports how many published events each category holds, so
	// the listing can show the count beside each one and grey out the empties.
	CategoryCounts(ctx context.Context) (map[Category]int64, error)
}

// CityCount is one city and how many published events it has.
type CityCount struct {
	City  string
	UF    string
	Count int64
}
