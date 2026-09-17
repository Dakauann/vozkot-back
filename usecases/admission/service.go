// Package admission is the door.
//
// One entry point that matters — Scan — and it is written for a queue of people
// waiting outside in the cold. Three things follow from that:
//
//   - It ALWAYS answers. A refusal is a result, not an error: "already used at
//     21:14" and "that is for the other stage" are the two things a doorperson
//     most needs to be told, and an HTTP 4xx with a generic message tells them
//     neither. Only a genuine fault — the database is gone — comes back as an
//     error.
//   - It is cheap. A malformed code is refused by arithmetic before any query
//     runs, and the happy path is one indexed read plus one conditional UPDATE.
//   - It is honest about order. Ownership of the door is checked BEFORE the code
//     is looked up, so an unauthorised caller cannot use this endpoint to learn
//     whether a code exists.
package admission

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	domain "vozkot/domain/admission"
	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	orderdomain "vozkot/domain/order"
	seatingdomain "vozkot/domain/seating"
)

// Outcome is what the door should show.
//
// A closed set of names the client branches on, rather than a message it
// displays: the phrasing belongs to the interface and the translation, and a
// scanner in a loud room shows a colour and a word, not a sentence.
type Outcome string

const (
	// OutcomeAdmitted is the only one that lets somebody in, and only ever
	// once per code.
	OutcomeAdmitted Outcome = "admitted"
	// OutcomeAlreadyAdmitted is the second scan of a code. The result carries
	// when and by whom, because that is the conversation that follows.
	OutcomeAlreadyAdmitted Outcome = "already_admitted"
	// OutcomeVoid is a code from a refunded or cancelled order.
	OutcomeVoid Outcome = "void"
	// OutcomeWrongEvent is a real ticket for another door.
	OutcomeWrongEvent Outcome = "wrong_event"
	// OutcomeNotPaid is a code whose order has stopped being paid without
	// having been refunded — an expired hold that was somehow issued against,
	// or a status moved by hand. It should not happen; it is answered rather
	// than hidden.
	OutcomeNotPaid Outcome = "not_paid"
	// OutcomeUnknown is a well-formed code nobody was ever issued.
	OutcomeUnknown Outcome = "unknown"
	// OutcomeMalformed is not a code at all: wrong length, bad characters, or
	// a failed check character. Answered without touching the database.
	OutcomeMalformed Outcome = "malformed"
)

// Admitted reports whether this outcome opened the door.
func (o Outcome) Admitted() bool { return o == OutcomeAdmitted }

// ScanResult is everything the door needs to draw its answer.
type ScanResult struct {
	Outcome Outcome
	// Seat is the reserved chair, empty for general admission.
	//
	// The single most useful thing the door gains from reserved seating: the
	// person in front of the scanner has just been told they may come in, and
	// the next thing they ask is where to sit.
	Seat seatingdomain.Label
	// TicketTitle is the tier, so the doorperson can see that the person in
	// front of them is holding a Camarote and send them to the right entrance.
	TicketTitle string
	// Sequence and OrderReference identify WHICH of a group's tickets this is,
	// which is what settles an argument about whether all four have been used.
	Sequence       int
	OrderReference string
	// AdmittedAt and AdmittedBy are set on OutcomeAlreadyAdmitted: the time is
	// what the holder is told, the account is what the operator audits.
	AdmittedAt *time.Time
	AdmittedBy string
	// Counters are the door's running totals for this event, returned with
	// every scan so the screen stays live without a second request.
	Remaining int
	Admitted  int
}

type Service struct {
	admissions domain.Repository
	orders     orderdomain.Repository
	events     eventdomain.Repository
	// codes renders a QR. Nil is supported and means the image endpoint
	// answers "not available" while the printed code still works.
	codes domain.CodeRenderer
	now   func() time.Time
}

func NewService(
	admissions domain.Repository,
	orders orderdomain.Repository,
	events eventdomain.Repository,
) *Service {
	return &Service{admissions: admissions, orders: orders, events: events, now: time.Now}
}

// WithCodeRenderer attaches the encoder used to draw a holder's QR.
func (s *Service) WithCodeRenderer(codes domain.CodeRenderer) *Service {
	if s != nil {
		s.codes = codes
	}
	return s
}

// Scan resolves a code at a door and spends it if it is good.
func (s *Service) Scan(
	ctx context.Context,
	actor authdomain.Actor,
	eventID, rawCode string,
) (ScanResult, error) {
	// The door first. An unauthorised caller must not be able to use this to
	// probe which codes exist, so nothing about the code is looked up until
	// the caller has been shown to run this event.
	happening, err := s.events.GetByID(ctx, strings.TrimSpace(eventID))
	if err != nil {
		return ScanResult{}, err
	}
	if err := happening.AuthorizeReach(actor); err != nil {
		return ScanResult{}, err
	}

	// Arithmetic before I/O. A scanner pointed at a beer label, or a
	// doorperson who mistyped, is answered here without a query.
	code, err := domain.ParseCode(rawCode)
	if err != nil {
		return s.decorate(ctx, happening.ID, ScanResult{Outcome: OutcomeMalformed}), nil
	}

	item, err := s.admissions.FindByCode(ctx, code)
	if errors.Is(err, domain.ErrNotFound) {
		return s.decorate(ctx, happening.ID, ScanResult{Outcome: OutcomeUnknown}), nil
	}
	if err != nil {
		return ScanResult{}, err
	}

	result := ScanResult{
		TicketTitle:    item.TicketTitle,
		Seat:           item.Seat,
		Sequence:       item.Sequence,
		OrderReference: orderdomain.Reference(item.OrderID),
		AdmittedAt:     item.AdmittedAt,
		AdmittedBy:     item.AdmittedBy,
	}

	// A real ticket at the wrong door. Its own answer, because a festival with
	// three stages generates this constantly and the holder needs directing
	// rather than accusing.
	if item.EventID != happening.ID {
		result.Outcome = OutcomeWrongEvent
		return s.decorate(ctx, happening.ID, result), nil
	}

	switch item.Status {
	case domain.StatusVoid:
		result.Outcome = OutcomeVoid
		return s.decorate(ctx, happening.ID, result), nil
	case domain.StatusAdmitted:
		result.Outcome = OutcomeAlreadyAdmitted
		return s.decorate(ctx, happening.ID, result), nil
	}

	// The order behind it is checked at SCAN time rather than trusted from the
	// admission's own status. Voiding on refund is the primary mechanism and it
	// runs in the refund's transaction, but a status moved by hand, a
	// half-applied migration or a bug in that path would otherwise leave a
	// working ticket for money that went back. This is the belt to that brace,
	// and it costs one indexed read on the way in.
	order, err := s.orders.GetByID(ctx, item.OrderID)
	if err != nil {
		return ScanResult{}, err
	}
	if order.Status != orderdomain.StatusPaid {
		log.Printf("admission: refusing %s at event %s; its order %s is %q, not paid",
			item.ID, happening.ID, order.ID, order.Status)
		result.Outcome = OutcomeNotPaid
		return s.decorate(ctx, happening.ID, result), nil
	}

	// The claim. One conditional UPDATE, and its answer is authoritative:
	// false means somebody else got there first, in which case the honest
	// reply is the one the loser of the race should hear.
	admitted, err := s.admissions.Admit(ctx, item.ID, actor.ID)
	if err != nil {
		return ScanResult{}, err
	}
	if !admitted {
		// Lost the race, by a millisecond or to another door. Re-read so the
		// answer carries the winner's timestamp rather than a guess.
		if fresh, freshErr := s.admissions.FindByCode(ctx, code); freshErr == nil {
			result.AdmittedAt = fresh.AdmittedAt
			result.AdmittedBy = fresh.AdmittedBy
			if fresh.Status == domain.StatusVoid {
				result.Outcome = OutcomeVoid
				return s.decorate(ctx, happening.ID, result), nil
			}
		}
		result.Outcome = OutcomeAlreadyAdmitted
		return s.decorate(ctx, happening.ID, result), nil
	}

	now := s.now().UTC()
	result.Outcome = OutcomeAdmitted
	result.AdmittedAt = &now
	result.AdmittedBy = actor.ID
	return s.decorate(ctx, happening.ID, result), nil
}

// Counters is the door's running total on its own, for a screen that polls.
func (s *Service) Counters(
	ctx context.Context,
	actor authdomain.Actor,
	eventID string,
) (remaining int, admitted int, err error) {
	happening, err := s.events.GetByID(ctx, strings.TrimSpace(eventID))
	if err != nil {
		return 0, 0, err
	}
	if err := happening.AuthorizeReach(actor); err != nil {
		return 0, 0, err
	}
	return s.admissions.CountAdmitted(ctx, happening.ID)
}

// ForOrder is the holder's own tickets, with their codes.
//
// Scoped to the buyer, not to the event's owner: these are the QR codes
// themselves, and an organiser who could read them could walk in on somebody
// else's ticket. An operator passes, for support.
func (s *Service) ForOrder(
	ctx context.Context,
	actor authdomain.Actor,
	orderID string,
) ([]domain.Admission, error) {
	order, err := s.orders.GetByID(ctx, strings.TrimSpace(orderID))
	if err != nil {
		return nil, err
	}
	if !actor.MayReach(order.BuyerID) {
		return nil, fmt.Errorf("%w: these tickets belong to another buyer", authdomain.ErrForbidden)
	}
	return s.admissions.ListByOrder(ctx, order.ID)
}

// decorate attaches the door's counters to whatever answer was reached.
//
// Best effort: a counter that cannot be read must not turn a successful
// admission into a failure, so the error is logged and the result stands. The
// person is already through the door by this point.
func (s *Service) decorate(ctx context.Context, eventID string, result ScanResult) ScanResult {
	remaining, admitted, err := s.admissions.CountAdmitted(ctx, eventID)
	if err != nil {
		log.Printf("admission: counters for event %s unavailable: %v", eventID, err)
		return result
	}
	result.Remaining = remaining
	result.Admitted = admitted
	return result
}

// QR renders the holder's own ticket as a PNG.
//
// Addressed by ADMISSION ID, never by code. The image has to be reachable from
// an <img src>, which means the address ends up in browser history, in a proxy
// log and in anything that records a URL; an id that is useless without a
// session is safe there and a code is not. The code itself is read from the
// sealed column on the server and never leaves it in a URL.
func (s *Service) QR(
	ctx context.Context,
	actor authdomain.Actor,
	orderID, admissionID string,
) ([]byte, error) {
	if s.codes == nil {
		return nil, fmt.Errorf("%w: no code renderer is configured", domain.ErrNotFound)
	}
	// Scoped through the order, so the same buyer-only rule guards the image
	// as guards the codes themselves. One method decides it, once.
	issued, err := s.ForOrder(ctx, actor, orderID)
	if err != nil {
		return nil, err
	}
	for index := range issued {
		if issued[index].ID != strings.TrimSpace(admissionID) {
			continue
		}
		return s.codes.PNG(issued[index].Code)
	}
	return nil, domain.ErrNotFound
}
