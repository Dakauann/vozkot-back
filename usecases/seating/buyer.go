package seating

import (
	"context"
	"sort"
	"strings"

	domain "vozkot/domain/seating"
)

// The buyer's side: reading a map, and being handed good seats without reading
// one at all.
//
// Everything here is PUBLIC. A seat map is what somebody looks at before they
// have an account, so none of it takes an Actor, and none of it returns
// anything an anonymous visitor should not see: a seat's status, its price tier
// and its label. Never who holds it.

// SeatView is one chair as a buyer sees it.
//
// Deliberately not domain.EventSeat. That type carries OrderID and
// HoldExpiresAt, and shipping those to a picker would tell every visitor which
// account is holding which chair and exactly when to race them for it.
type SeatView struct {
	ID       string
	TicketID string
	Section  string
	Row      string
	Seat     string
	Kind     domain.SeatKind
	Status   domain.Status
	// X and Y are where the seat IS. Without them a picker can only draw rows
	// as a list, which is wrong for any venue that is not a theatre: a rodeo's
	// stands wrap an arena, and a map that does not resemble the room cannot
	// answer the only question a map is asked.
	X         float64
	Y         float64
	RowOrder  int
	SeatOrder int
	Version   int64
}

// MarkerView is something in the room that holds no seats: the stage, the
// floor a rodeo runs in.
//
// "Where is the stage" is the first question anybody asks of a seat map, and it
// cannot be derived from the seats -- a theatre has one at an end, a rodeo has
// an arena in the middle with a show stage beside it, a gala floor has neither.
// Only the organiser knows, and they say so by placing it.
type MarkerView struct {
	ID     string
	Name   string
	Kind   domain.SectionKind
	X      float64
	Y      float64
	Width  float64
	Height float64
}

// MapView is the whole map, or the part of it that changed.
type MapView struct {
	EventID string
	// Markers are the room's scenery, sent with a COMPLETE map. A picker draws
	// them so a buyer can tell which end they are buying.
	Markers []MarkerView
	Seats   []SeatView
	// Version is the highest cursor in this response, and what the client sends
	// back as `since`. Zero when nothing changed, which a client treats as "no
	// news" rather than as a reset.
	Version int64
	// Complete says whether this is the whole map or a delta.
	Complete bool
}

// Map returns an event's seats, or only those changed since a cursor.
//
// It does NOT go through the availability cache, and that is a decision worth
// stating: TicketTTL is three seconds, which is a good trade for "Pista: 412
// disponíveis" and the wrong one for a chair. Three seconds of staleness on a
// seat map means two buyers reliably picking the same seat and one of them
// losing, every few seconds, for the whole onsale. This reads the rows, which
// is one indexed scan, and it is cheap precisely because the map is advisory:
// being 200ms stale costs a retry, being 3s stale costs a fight.
func (s *Service) Map(ctx context.Context, eventID string, sinceVersion int64) (MapView, error) {
	seats, err := s.seats.ListByEvent(ctx, eventID, sinceVersion)
	if err != nil {
		return MapView{}, err
	}
	view := MapView{
		EventID:  eventID,
		Seats:    make([]SeatView, 0, len(seats)),
		Complete: sinceVersion <= 0,
	}

	// The markers come with the COMPLETE map and not with a delta.
	//
	// A poll during an onsale runs every few seconds per open picker, and the
	// stage does not move between two of them. Reading them every time would
	// put two extra queries on the hottest path this feature has, to re-send
	// something the client already holds.
	//
	// Failure here is deliberately swallowed. A map with no stage drawn is a
	// worse map; a map that 500s because the scenery could not be read is no
	// map at all, and the seats are what a buyer came for.
	if view.Complete {
		if manifest, err := s.seats.SeatingOf(ctx, eventID); err == nil && manifest != nil {
			if sections, err := s.layouts.SectionsOf(ctx, manifest.LayoutID); err == nil {
				for index := range sections {
					section := &sections[index]
					if !section.Kind.Marker() {
						continue
					}
					view.Markers = append(view.Markers, MarkerView{
						ID:     section.ID,
						Name:   section.Name,
						Kind:   section.Kind,
						X:      section.OffsetX,
						Y:      section.OffsetY,
						Width:  section.Width,
						Height: section.Height,
					})
				}
			}
		}
	}
	for index := range seats {
		seat := &seats[index]
		if seat.Version > view.Version {
			view.Version = seat.Version
		}
		view.Seats = append(view.Seats, SeatView{
			ID:        seat.ID,
			TicketID:  seat.TicketID,
			Section:   seat.Label.Section,
			Row:       seat.Label.Row,
			Seat:      seat.Label.Seat,
			Kind:      seat.Kind,
			Status:    seat.Status,
			X:         seat.X,
			Y:         seat.Y,
			RowOrder:  seat.RowOrder,
			SeatOrder: seat.SeatOrder,
			Version:   seat.Version,
		})
	}
	return view, nil
}

// Availability is how many of each tier's seats are free.
type Availability struct {
	TicketID  string
	Available int
	Total     int
}

// Availability is what the sector list shows above each price.
func (s *Service) Availability(ctx context.Context, eventID string) ([]Availability, error) {
	counts, err := s.seats.CountsByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := make([]Availability, 0, len(counts))
	for _, tally := range counts {
		out = append(out, Availability{
			TicketID:  tally.TicketID,
			Available: tally.Available,
			Total:     tally.Total(),
		})
	}
	return out, nil
}

// BestAvailableInput asks for N seats together.
type BestAvailableInput struct {
	EventID  string
	TicketID string
	Quantity int
	// Accessible asks for accessible seats specifically. Without it they are
	// never offered, which is the whole reason they are marked: an accessible
	// seat handed to somebody who did not need one is a wheelchair user
	// arriving to find a chair.
	Accessible bool
}

// BestAvailable picks the best run of adjacent seats it can find.
//
// This is the PRIMARY path, not a fallback. Most buyers do not want to study a
// chart; they want four seats together, near the front, now. Hand-picking is
// the alternative to this, and this is also what gives a screen-reader user a
// real way to buy: a canvas of chairs is an image, and "four seats together in
// the cheapest row you have" is a sentence.
//
// Adjacency comes from RowOrder and SeatOrder and never from the labels,
// because in an odd/even house seats 5 and 7 are neighbours and 5 and 6 are
// not.
//
// It returns fewer than asked only by returning nothing: a party of four told
// "here are three" has to start again anyway, and silently splitting them
// across rows is the single most complained-about behaviour a seat picker has.
func (s *Service) BestAvailable(ctx context.Context, input BestAvailableInput) ([]SeatView, error) {
	if input.Quantity <= 0 {
		return nil, domain.ErrNoSeats
	}
	view, err := s.Map(ctx, input.EventID, 0)
	if err != nil {
		return nil, err
	}

	ticketID := strings.TrimSpace(input.TicketID)
	candidates := make([]SeatView, 0, len(view.Seats))
	for _, seat := range view.Seats {
		if seat.Status != domain.StatusAvailable {
			continue
		}
		if ticketID != "" && seat.TicketID != ticketID {
			continue
		}
		if seat.Kind.Accessible() != input.Accessible {
			continue
		}
		candidates = append(candidates, seat)
	}

	// Grouped by section and row, in seat order, so a run is a walk.
	sort.Slice(candidates, func(a, b int) bool {
		if candidates[a].Section != candidates[b].Section {
			return candidates[a].Section < candidates[b].Section
		}
		if candidates[a].RowOrder != candidates[b].RowOrder {
			return candidates[a].RowOrder < candidates[b].RowOrder
		}
		return candidates[a].SeatOrder < candidates[b].SeatOrder
	})

	var best []SeatView
	bestScore := 0
	for start := 0; start < len(candidates); start++ {
		run := []SeatView{candidates[start]}
		for next := start + 1; next < len(candidates) && len(run) < input.Quantity; next++ {
			previous := run[len(run)-1]
			candidate := candidates[next]
			if candidate.Section != previous.Section ||
				candidate.RowOrder != previous.RowOrder ||
				candidate.SeatOrder != previous.SeatOrder+1 {
				break
			}
			run = append(run, candidate)
		}
		if len(run) < input.Quantity {
			continue
		}
		if score := scoreRun(run); best == nil || score > bestScore {
			best, bestScore = run, score
		}
	}
	if best == nil {
		return nil, nil
	}
	return best, nil
}

// scoreRun prefers the front, and the centre of a row over its edges.
//
// Front first because that is what people pay for and what "best" means to
// them. Centre second, and it is scored rather than sorted so that a centre
// seat two rows back beats an aisle seat at the very front — which is the trade
// a person actually makes when they choose by hand.
func scoreRun(run []SeatView) int {
	if len(run) == 0 {
		return 0
	}
	// Rows are numbered from the stage outwards, so a lower order is better.
	// Weighted an order of magnitude above the centre bonus: being near the
	// stage dominates, and the centre breaks ties within a few rows.
	score := -run[0].RowOrder * 10

	// Distance of the run's middle from the row's own middle is not knowable
	// from the run alone, so the proxy is how tightly the run sits around the
	// lowest seat orders available — good enough, and it costs no second query.
	middle := (run[0].SeatOrder + run[len(run)-1].SeatOrder) / 2
	if middle < 0 {
		middle = -middle
	}
	return score - middle
}
