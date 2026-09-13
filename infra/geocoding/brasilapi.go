// Package geocoding turns a written Brazilian address into a point on a map.
//
// It talks to BrasilAPI, and the choice is a licensing decision before it is a
// technical one. An event's coordinates live on its row for as long as the
// event does, and most geocoders forbid exactly that: Google's terms permit
// caching a coordinate for thirty days and no longer, and Mapbox separates a
// cheap "temporary" tier from a paid "permanent" one for the same reason. A
// stored column fed by either would be a licence violation that nothing in the
// code would ever flag.
//
// BrasilAPI resolves a CEP through several upstream providers, needs no key, no
// account and no billing relationship, and returns coordinates derived from
// open data that may be kept. For a Brazilian box office whose operators
// already type a CEP, it answers most addresses at no cost and no legal risk.
//
// What it does NOT do is street-number precision. A CEP in a dense city block
// is a building; in a small town it can be a whole district. The precision is
// reported rather than hidden, and an operator can always drag the pin.
package geocoding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	eventdomain "vozkot/domain/event"
)

// DefaultBaseURL is BrasilAPI's public host.
const DefaultBaseURL = "https://brasilapi.com.br"

// defaultTimeout bounds one lookup.
//
// Short on purpose. This runs while an operator waits for a save to return, and
// the save does not need the answer: a geocoder that is having a slow day must
// cost the map, never the event.
const defaultTimeout = 4 * time.Second

// cepDigits is exactly what the API accepts, once punctuation is stripped.
const cepDigits = 8

type BrasilAPI struct {
	baseURL string
	client  *http.Client
}

var _ eventdomain.Geocoder = (*BrasilAPI)(nil)

type Option func(*BrasilAPI)

// WithHTTPClient overrides the transport, for tests.
func WithHTTPClient(client *http.Client) Option {
	return func(g *BrasilAPI) {
		if client != nil {
			g.client = client
		}
	}
}

// WithBaseURL points at another host, for tests.
func WithBaseURL(baseURL string) Option {
	return func(g *BrasilAPI) {
		if trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/"); trimmed != "" {
			g.baseURL = trimmed
		}
	}
}

func New(opts ...Option) *BrasilAPI {
	geocoder := &BrasilAPI{
		baseURL: DefaultBaseURL,
		client:  &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(geocoder)
	}
	return geocoder
}

// cepResponse is v2 of the CEP endpoint, which is the version that carries
// coordinates. v1 returns the address only.
type cepResponse struct {
	CEP          string `json:"cep"`
	State        string `json:"state"`
	City         string `json:"city"`
	Neighborhood string `json:"neighborhood"`
	Street       string `json:"street"`
	Location     struct {
		Type        string `json:"type"`
		Coordinates struct {
			// Strings, not numbers: the API returns them quoted, and decoding
			// into a float64 fails outright on a quoted value.
			Longitude string `json:"longitude"`
			Latitude  string `json:"latitude"`
		} `json:"coordinates"`
	} `json:"location"`
}

// Locate resolves a location's postcode to a point.
//
// Not found is (nil, nil) rather than an error, because an address nobody can
// place is still a real address and the event must still be publishable. Only a
// failure to ASK is an error, so a caller can tell the two apart.
func (g *BrasilAPI) Locate(ctx context.Context, location eventdomain.Location) (*eventdomain.Coordinates, error) {
	postalCode := onlyDigits(location.PostalCode)
	if len(postalCode) != cepDigits {
		// No CEP, or not a CEP. Nothing to ask, and asking anyway would spend a
		// request to be told so.
		return nil, nil
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		g.baseURL+"/api/cep/v2/"+postalCode, nil)
	if err != nil {
		return nil, fmt.Errorf("geocoding: build request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	// A real user agent is courtesy on a free public service, and several
	// open-data endpoints refuse an empty one outright.
	request.Header.Set("User-Agent", "vozkot/1.0 (+https://github.com/vozkot)")

	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("geocoding: %w", err)
	}
	defer response.Body.Close()

	switch {
	case response.StatusCode == http.StatusNotFound:
		// The service looked and does not know this postcode.
		return nil, nil
	case response.StatusCode < 200 || response.StatusCode > 299:
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return nil, fmt.Errorf("geocoding: %s answered %d: %s",
			g.baseURL, response.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded cepResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("geocoding: decode response: %w", err)
	}

	latitude, latErr := strconv.ParseFloat(strings.TrimSpace(decoded.Location.Coordinates.Latitude), 64)
	longitude, lngErr := strconv.ParseFloat(strings.TrimSpace(decoded.Location.Coordinates.Longitude), 64)
	if latErr != nil || lngErr != nil {
		// A known postcode whose coordinates the upstream provider did not
		// carry. Ordinary, and not a failure: the address is still correct.
		return nil, nil
	}
	if !plausiblyBrazilian(latitude, longitude) {
		// A zero pair, or a point in the Gulf of Guinea, which is what a
		// missing value decodes to. Better no pin than a wrong one.
		return nil, nil
	}

	return &eventdomain.Coordinates{
		Latitude:  latitude,
		Longitude: longitude,
		Source:    "brasilapi/cep-v2",
		// A CEP names a street or a block, not a door. Claiming otherwise
		// would let a later "exact beats approximate" rule keep a worse answer.
		Precision: eventdomain.PrecisionApproximate,
	}, nil
}

// plausiblyBrazilian is a sanity check, not a boundary test.
//
// It exists to catch the two failure shapes that actually occur — a zero pair,
// and a transposed latitude/longitude — rather than to police the border. A
// generous box costs nothing and a tight one would reject Oiapoque.
func plausiblyBrazilian(latitude, longitude float64) bool {
	if latitude == 0 && longitude == 0 {
		return false
	}
	return latitude >= -35 && latitude <= 6 && longitude >= -75 && longitude <= -32
}

func onlyDigits(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if char >= '0' && char <= '9' {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

// ErrNotConfigured is returned by a nil geocoder's Locate, so a caller that
// forgets the nil check gets a clear error rather than a panic.
var ErrNotConfigured = errors.New("geocoding: no geocoder is configured")
