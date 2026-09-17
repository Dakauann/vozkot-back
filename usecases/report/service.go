// Package report is the organiser's view of who bought their tickets.
//
// Everything here is a READ, and every read is scoped to an event the caller
// owns. That scoping is the entire security surface of this package: the
// numbers themselves are harmless, the list is not, and the difference between
// an organiser seeing their own audience and seeing somebody else's is one
// forgotten ownership check.
//
// So the check is in ONE place — authorise — and every exported method starts
// with it. Not in the handler: a rule enforced at the transport edge is a rule
// the next caller forgets.
package report

import (
	"context"
	"errors"
	"strings"

	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	domain "vozkot/domain/report"
)

type Service struct {
	reports domain.Repository
	events  eventdomain.Repository
}

func NewService(reports domain.Repository, events eventdomain.Repository) *Service {
	return &Service{reports: reports, events: events}
}

// Sales is the dashboard: totals plus every breakdown, for one event.
func (s *Service) Sales(ctx context.Context, eventID string, actor authdomain.Actor) (domain.Sales, error) {
	happening, err := s.authorise(ctx, eventID, actor)
	if err != nil {
		return domain.Sales{}, err
	}
	sales, err := s.reports.Sales(ctx, happening.ID)
	if err != nil {
		return domain.Sales{}, err
	}
	sales.EventID = happening.ID
	return sales, nil
}

// Attendees is the paginated list behind the table.
func (s *Service) Attendees(
	ctx context.Context,
	filter domain.AttendeeFilter,
	actor authdomain.Actor,
) (domain.Page, error) {
	prepared, err := s.prepare(ctx, filter, actor)
	if err != nil {
		return domain.Page{}, err
	}
	return s.reports.Attendees(ctx, prepared)
}

// Export streams every matching row to fn, for the CSV.
//
// A callback rather than a slice: an arena is fifty thousand orders and this
// must not hold all of them in memory to write them one at a time to a socket.
func (s *Service) Export(
	ctx context.Context,
	filter domain.AttendeeFilter,
	actor authdomain.Actor,
	fn func([]domain.Attendee) error,
) (*eventdomain.Event, error) {
	happening, err := s.authorise(ctx, filter.EventID, actor)
	if err != nil {
		return nil, err
	}
	prepared := s.defaults(filter)
	prepared.EventID = happening.ID
	if err := s.reports.StreamAttendees(ctx, prepared, fn); err != nil {
		return nil, err
	}
	// The event comes back so the handler can name the file after the show
	// rather than after its id: "participantes-festival-x.csv" is what an
	// organiser will still recognise in their downloads folder next week.
	return happening, nil
}

// prepare authorises and normalises a listing filter.
func (s *Service) prepare(
	ctx context.Context,
	filter domain.AttendeeFilter,
	actor authdomain.Actor,
) (domain.AttendeeFilter, error) {
	happening, err := s.authorise(ctx, filter.EventID, actor)
	if err != nil {
		return domain.AttendeeFilter{}, err
	}
	prepared := s.defaults(filter).Normalize()
	prepared.EventID = happening.ID
	return prepared, nil
}

// defaults pins the one filter value that must not be left to the caller.
//
// An attendee list defaults to PAID orders. "Who is coming" is a question about
// people who actually paid, and a list that quietly included expired holds
// would have an organiser emailing a stadium's worth of people who never bought
// anything. A caller may still ask for another status explicitly — reconciling
// refunds needs exactly that — but they have to ask.
func (s *Service) defaults(filter domain.AttendeeFilter) domain.AttendeeFilter {
	if strings.TrimSpace(filter.Status) == "" {
		filter.Status = "paid"
	}
	if strings.EqualFold(filter.Status, "all") {
		filter.Status = ""
	}
	return filter
}

// authorise resolves the event and refuses one that is not the caller's.
//
// An administrator passes, which is what makes support possible. Everybody else
// must own the event: not merely be signed in, and not merely have bought a
// ticket for it.
func (s *Service) authorise(
	ctx context.Context,
	eventID string,
	actor authdomain.Actor,
) (*eventdomain.Event, error) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return nil, eventdomain.ErrNotFound
	}
	happening, err := s.events.GetByID(ctx, eventID)
	if err != nil {
		return nil, err
	}
	// ErrForbidden and not ErrNotFound, deliberately. Hiding the existence of
	// an event here buys nothing — the catalogue is public — and a 404 for an
	// event the organiser can see on their own listing is the kind of answer
	// that generates a support ticket instead of a correction.
	if err := happening.AuthorizeReach(actor); err != nil {
		return nil, err
	}
	return happening, nil
}

// IsForbidden reports whether an error is an ownership refusal.
func IsForbidden(err error) bool { return errors.Is(err, authdomain.ErrForbidden) }
