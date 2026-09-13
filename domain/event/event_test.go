package event

import (
	"errors"
	"testing"
	"time"
)

func validDraft() Draft {
	return Draft{
		Name:     "Festival Aurora",
		Category: CategoryFestasShows,
		Location: Location{
			Venue:   "Arena Castelão",
			Address: "Av. Alberto Craveiro, 2901",
			City:    "Fortaleza",
			UF:      "CE",
		},
		StartsAt: time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC),
	}
}

func TestNewStartsAsADraft(t *testing.T) {
	item, err := New("evt_1", "usr_1", validDraft(), time.Now())

	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if item.Status != StatusDraft {
		t.Fatalf("status = %q, want %q: a new event must not reach buyers before someone publishes it", item.Status, StatusDraft)
	}
	if item.Slug != "festival-aurora" {
		t.Fatalf("slug = %q, want festival-aurora", item.Slug)
	}
}

func TestNewRejectsIncompleteDrafts(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Draft)
		want   error
	}{
		"no name":        {func(d *Draft) { d.Name = "   " }, ErrInvalidName},
		"no venue":       {func(d *Draft) { d.Location.Venue = "" }, ErrInvalidVenue},
		"no city":        {func(d *Draft) { d.Location.City = "" }, ErrInvalidCity},
		"no start":       {func(d *Draft) { d.StartsAt = time.Time{} }, ErrInvalidStartsAt},
		"unknown kind":   {func(d *Draft) { d.Category = "raves" }, ErrInvalidCategory},
		"bad status":     {func(d *Draft) { d.Status = "live" }, ErrInvalidStatus},
		"ends too early": {func(d *Draft) { before := d.StartsAt.Add(-time.Hour); d.EndsAt = &before }, ErrEndsBeforeStarts},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			draft := validDraft()
			testCase.mutate(&draft)

			if _, err := New("evt_1", "usr_1", draft, time.Now()); !errors.Is(err, testCase.want) {
				t.Fatalf("New() error = %v, want %v", err, testCase.want)
			}
		})
	}
}

// A half-set coordinate pair would put the pin in the Atlantic, so the pair is
// all-or-nothing.
func TestCoordinatesAreSetTogetherOrNotAtAll(t *testing.T) {
	latitude := -3.807
	longitude := -38.522

	only := validDraft()
	only.Location.Latitude = &latitude
	if _, err := New("evt_1", "usr_1", only, time.Now()); !errors.Is(err, ErrInvalidLocation) {
		t.Fatalf("New() with only a latitude error = %v, want %v", err, ErrInvalidLocation)
	}

	outOfRange := validDraft()
	tooFar := 91.0
	outOfRange.Location.Latitude = &tooFar
	outOfRange.Location.Longitude = &longitude
	if _, err := New("evt_1", "usr_1", outOfRange, time.Now()); !errors.Is(err, ErrInvalidLocation) {
		t.Fatalf("New() with latitude 91 error = %v, want %v", err, ErrInvalidLocation)
	}

	both := validDraft()
	both.Location.Latitude = &latitude
	both.Location.Longitude = &longitude
	item, err := New("evt_1", "usr_1", both, time.Now())
	if err != nil {
		t.Fatalf("New() with a full pair error = %v", err)
	}
	if !item.Location.HasCoordinates() {
		t.Fatal("a full coordinate pair did not register as mappable")
	}
}

// An event with no coordinates is still a real event. Refusing it would mean a
// venue no geocoder recognises could never be sold.
func TestAnEventWithoutCoordinatesIsValid(t *testing.T) {
	item, err := New("evt_1", "usr_1", validDraft(), time.Now())

	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if item.Location.HasCoordinates() {
		t.Fatal("an event with no coordinates reported that it had some")
	}
}

// The slug is the public address. Correcting a typo in a name must not break
// every link already shared.
func TestApplyNeverChangesTheSlug(t *testing.T) {
	item, err := New("evt_1", "usr_1", validDraft(), time.Now())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	original := item.Slug

	renamed := validDraft()
	renamed.Name = "Festival Aurora 2027"
	if err := item.Apply(renamed, time.Now()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if item.Name != "Festival Aurora 2027" {
		t.Fatalf("name = %q, want the correction to land", item.Name)
	}
	if item.Slug != original {
		t.Fatalf("slug = %q, want it unchanged at %q: every shared link would 404", item.Slug, original)
	}
}

func TestApplyKeepsTheCurrentStatusWhenNoneIsGiven(t *testing.T) {
	item, _ := New("evt_1", "usr_1", validDraft(), time.Now())
	item.Status = StatusPublished

	if err := item.Apply(validDraft(), time.Now()); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if item.Status != StatusPublished {
		t.Fatalf("status = %q, want an edit that says nothing about status to leave it alone", item.Status)
	}
}

// Accents are folded rather than dropped: a Brazilian catalogue where every
// other name loses a letter is one nobody can guess a URL in.
func TestSlugifyFoldsPortugueseAccents(t *testing.T) {
	cases := map[string]string{
		"Festival Aurora":          "festival-aurora",
		"Sertão Sessions":          "sertao-sessions",
		"Coração & Alma":           "coracao-alma",
		"  Espaço   das   Artes  ": "espaco-das-artes",
		"Réveillon 2027":           "reveillon-2027",
		"Ação/Reação":              "acao-reacao",
		"---":                      "evento",
		"":                         "evento",
	}

	for name, want := range cases {
		if got := Slugify(name); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSlugifyBoundsItsLength(t *testing.T) {
	long := ""
	for index := 0; index < 40; index++ {
		long += "festival "
	}

	slug := Slugify(long)

	if len(slug) > 80 {
		t.Fatalf("slug length = %d, want at most 80", len(slug))
	}
	if slug[len(slug)-1] == '-' {
		t.Fatalf("slug = %q, want no trailing dash after truncation", slug)
	}
}

// The taxonomy is fixed so a listing can render an icon per category and a
// filter cannot list four spellings of "show".
func TestTheTaxonomyIsClosed(t *testing.T) {
	if !CategoryFestasShows.Valid() || !CategoryOutros.Valid() {
		t.Fatal("a known category reported itself invalid")
	}
	if Category("gratis").Valid() {
		t.Fatal(`"gratis" is a filter, not a category: an event is not free INSTEAD of being a show`)
	}
	if Category("").Valid() {
		t.Fatal("the empty category is not a category")
	}

	seen := map[Category]bool{}
	for _, category := range Categories() {
		if seen[category] {
			t.Fatalf("category %q is listed twice", category)
		}
		seen[category] = true
	}
	if len(Categories()) != 16 {
		t.Fatalf("taxonomy size = %d, want 16", len(Categories()))
	}
}

// An operator who picks nothing gets the escape hatch rather than a refusal:
// without one they pick the nearest wrong answer and the filter starts lying.
func TestAnUnsetCategoryFallsBackToOutros(t *testing.T) {
	draft := validDraft()
	draft.Category = ""

	item, err := New("evt_1", "usr_1", draft, time.Now())

	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if item.Category != CategoryOutros {
		t.Fatalf("category = %q, want %q", item.Category, CategoryOutros)
	}
}

func TestFilterNormalizeBoundsWhatOneRequestMayAsk(t *testing.T) {
	// An unbounded page size is how a public listing becomes a way to download
	// the whole catalogue in one request.
	if got := (Filter{Limit: 5000}).Normalize().Limit; got != MaxPageSize {
		t.Fatalf("limit = %d, want it capped at %d", got, MaxPageSize)
	}
	if got := (Filter{}).Normalize().Limit; got != DefaultPageSize {
		t.Fatalf("limit = %d, want the default %d", got, DefaultPageSize)
	}
	// Offset pagination re-walks every row it skips, so page 200,000 is a
	// denial of service wearing a page number.
	if got := (Filter{Offset: 1_000_000}).Normalize().Offset; got != MaxOffset {
		t.Fatalf("offset = %d, want it capped at %d", got, MaxOffset)
	}
	if got := (Filter{Offset: -5}).Normalize().Offset; got != 0 {
		t.Fatalf("offset = %d, want 0", got)
	}
	// Relevance with no terms is not an order, it is a coin toss.
	if got := (Filter{Sort: SortRelevance}).Normalize().Sort; got != SortStartsAt {
		t.Fatalf("sort = %q, want %q when there is nothing to rank", got, SortStartsAt)
	}
	if got := (Filter{Sort: SortRelevance, Query: "rock"}).Normalize().Sort; got != SortRelevance {
		t.Fatalf("sort = %q, want relevance to survive when there are terms", got)
	}
	if got := (Filter{Sort: "cheapest"}).Normalize().Sort; got != SortStartsAt {
		t.Fatalf("sort = %q, want an unknown sort to fall back to %q", got, SortStartsAt)
	}
}
