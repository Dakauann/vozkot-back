// Package admission is one person's right to walk through a door.
//
// An order is money; an admission is entry. One paid order line for three
// tickets is THREE admissions, because three people arrive separately and each
// needs something of their own to present. Issuing one credential per order and
// counting heads at the door is how a group of three becomes an argument.
//
// The entity here owns two rules and nothing else:
//
//   - an admission is single use, and the transition that spends it is the only
//     one that may report success;
//   - an admission whose order stopped being paid is not entry any more.
//
// Everything about WHO may scan, and the atomicity that makes two simultaneous
// scans admit exactly once, lives outside: the first in usecases/admission, the
// second in the repository's conditional UPDATE. This package cannot enforce
// either and does not pretend to.
package admission

import (
	"errors"
	"strings"
	"time"

	"vozkot/domain/seating"
)

// Status is where an admission stands.
type Status string

const (
	// StatusIssued is entry that has not been used.
	StatusIssued Status = "issued"
	// StatusAdmitted is entry that has been spent. Terminal.
	StatusAdmitted Status = "admitted"
	// StatusVoid is entry withdrawn because the order behind it was refunded
	// or cancelled. Terminal, and deliberately NOT deletion: a voided
	// admission that somebody presents at a door has to be answerable with
	// "this was refunded on the 14th" rather than with "no such ticket",
	// which is what a doorperson needs to say to the person in front of them.
	StatusVoid Status = "void"
)

func (s Status) Valid() bool {
	switch s {
	case StatusIssued, StatusAdmitted, StatusVoid:
		return true
	default:
		return false
	}
}

// Spent reports whether this admission can still let somebody in.
func (s Status) Spent() bool { return s == StatusAdmitted || s == StatusVoid }

var (
	ErrNotFound = errors.New("admission not found")
	// ErrAlreadyAdmitted is a code presented twice. The second presentation is
	// not an error in the system; it is the system working.
	ErrAlreadyAdmitted = errors.New("this admission has already been used")
	// ErrVoid is a code from an order that was refunded or cancelled.
	ErrVoid = errors.New("this admission is no longer valid")
	// ErrWrongEvent is a valid code presented at the wrong door.
	//
	// Its own error rather than a not-found, because it is a real thing that
	// happens — a festival with three stages, a venue running two shows in one
	// night — and the doorperson needs to be told to send the holder next door
	// rather than told their ticket is fake.
	ErrWrongEvent = errors.New("this admission is for another event")
	// ErrNotPayable is an attempt to issue against an order that has not paid.
	ErrNotPayable   = errors.New("admissions are only issued for a paid order")
	ErrInvalidOrder = errors.New("an admission must name the order it was issued for")
	ErrInvalidEvent = errors.New("an admission must name the event it admits to")
	ErrInvalidTier  = errors.New("an admission must name the tier it was sold as")
)

// Admission is one credential.
//
// Code is present only when the admission has just been issued or has been
// deliberately re-read for its holder: the stored form is encrypted and its
// searchable companion is a blind index, exactly as a buyer's document is in
// domain/user. A doorperson's scan finds the row by the blind index and never
// needs the code back, so the normal read path leaves this empty.
type Admission struct {
	ID      string
	OrderID string
	EventID string
	// TicketID is the tier this admission was sold as, so a door can tell
	// Pista from Camarote and a report can group by what people bought.
	TicketID string
	// TicketTitle is the tier's name frozen at issue, for the same reason an
	// order line freezes it: renaming a tier must not rewrite a ticket
	// somebody is holding.
	TicketTitle string
	// Sequence numbers the admissions within one order line, from 1, so a
	// ticket can say "2 of 3" and a holder can tell their three apart.
	Sequence int
	// SeatID and Seat are the reserved chair, frozen at issue, and empty for a
	// general-admission ticket.
	//
	// The snapshot is the point: a venue that re-letters row I next season must
	// not rewrite the seat printed on a ticket somebody already holds, and the
	// door has to be able to read it without joining anything.
	SeatID string
	Seat   seating.Label

	Code   Code
	Status Status

	AdmittedAt *time.Time
	// AdmittedBy is the account that scanned it, for the audit an operator
	// needs when a holder disputes being turned away.
	AdmittedBy string
	VoidedAt   *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Draft is what the issuer supplies. The code is minted here, not passed in.
type Draft struct {
	OrderID     string
	EventID     string
	TicketID    string
	TicketTitle string
	Sequence    int
	// SeatID and Seat are the reserved chair, empty for general admission.
	SeatID string
	Seat   seating.Label
}

// New mints one admission with a fresh code.
func New(id string, draft Draft, now time.Time) (*Admission, error) {
	if strings.TrimSpace(draft.OrderID) == "" {
		return nil, ErrInvalidOrder
	}
	if strings.TrimSpace(draft.EventID) == "" {
		return nil, ErrInvalidEvent
	}
	if strings.TrimSpace(draft.TicketID) == "" {
		return nil, ErrInvalidTier
	}
	if draft.Sequence < 1 {
		draft.Sequence = 1
	}

	code, err := NewCode()
	if err != nil {
		return nil, err
	}

	timestamp := now.UTC()
	return &Admission{
		ID:          id,
		OrderID:     strings.TrimSpace(draft.OrderID),
		EventID:     strings.TrimSpace(draft.EventID),
		TicketID:    strings.TrimSpace(draft.TicketID),
		TicketTitle: strings.TrimSpace(draft.TicketTitle),
		Sequence:    draft.Sequence,
		SeatID:      strings.TrimSpace(draft.SeatID),
		Seat:        draft.Seat,
		Code:        code,
		Status:      StatusIssued,
		CreatedAt:   timestamp,
		UpdatedAt:   timestamp,
	}, nil
}

// Admit spends this admission.
//
// It reports the refusal a door needs to hear, and it is NOT what makes a scan
// atomic: two callers holding two copies of the same row would both pass here.
// The repository's conditional UPDATE is the thing that lets exactly one of
// them through, and this method exists so the decision is written down in the
// domain and testable without a database.
func (a *Admission) Admit(by string, now time.Time) error {
	switch a.Status {
	case StatusAdmitted:
		return ErrAlreadyAdmitted
	case StatusVoid:
		return ErrVoid
	}

	timestamp := now.UTC()
	a.Status = StatusAdmitted
	a.AdmittedAt = &timestamp
	a.AdmittedBy = strings.TrimSpace(by)
	a.UpdatedAt = timestamp
	return nil
}

// Void withdraws this admission, and reports whether that changed anything.
//
// Idempotent, because the thing that calls it is a refund, and a refund can be
// settled twice by a redelivered webhook. Voiding an admission that was
// already ADMITTED is allowed and is not a contradiction: the holder came in,
// then the money went back, and both facts are true. AdmittedAt is left
// standing so the report still shows they attended.
func (a *Admission) Void(now time.Time) bool {
	if a.Status == StatusVoid {
		return false
	}
	timestamp := now.UTC()
	a.Status = StatusVoid
	a.VoidedAt = &timestamp
	a.UpdatedAt = timestamp
	return true
}

// Usable reports whether presenting this would get somebody in right now.
func (a *Admission) Usable() bool { return a.Status == StatusIssued }
