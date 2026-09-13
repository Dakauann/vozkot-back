package event

import "context"

// Coordinates are a point on the map.
type Coordinates struct {
	Latitude  float64
	Longitude float64
	// Source names who answered, because the answers are not equally good and
	// an operator correcting a pin by hand must never be overwritten by a
	// geocoder that disagrees. See Precision.
	Source string
	// Precision says how tightly the answer is pinned.
	Precision Precision
}

// Precision is how much the coordinate can be trusted.
//
// It exists because a Brazilian postcode resolves to anything from a single
// building to a whole district, and a map pin that claims a street it cannot
// know is worse than one that admits it is approximate.
type Precision string

const (
	// PrecisionExact came from a rooftop or street-number match.
	PrecisionExact Precision = "exact"
	// PrecisionApproximate came from a postcode or a neighbourhood centroid.
	PrecisionApproximate Precision = "approximate"
	// PrecisionManual was placed by a person dragging the pin. It outranks
	// everything: a human looking at a satellite image beats any geocoder, and
	// a later edit to an unrelated field must not silently move it back.
	PrecisionManual Precision = "manual"
)

// Geocoder turns a written address into a point.
//
// The port is deliberately small and deliberately forgiving. Its one
// implementation talks to a third party over the network, which means it is
// sometimes slow, sometimes down, and sometimes simply does not know — and an
// event whose address no geocoder recognises is still a real event that must be
// possible to publish. So:
//
//   - Not found is (nil, nil), not an error. The caller stores no coordinates
//     and the page shows the address without a map.
//   - A transport failure IS an error, so a caller can tell "nobody knows where
//     this is" from "we could not ask", and retry the second.
//
// Storing what comes back is a licensing question as much as a technical one.
// Google's terms allow caching a coordinate for thirty days and no longer,
// which rules it out as the source of a column that lives as long as the event
// does. The adapter behind this port must be one whose terms permit permanent
// storage.
type Geocoder interface {
	// Locate finds the coordinates for a location, or reports that it cannot.
	Locate(ctx context.Context, location Location) (*Coordinates, error)
}
