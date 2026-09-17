package admission

import (
	"context"

	"vozkot/domain/seating"
)

// Repository is the persistence port. Only infra implements it, and only use
// cases call it.
type Repository interface {
	// IssueForOrder mints and stores the admissions a paid order is owed, and
	// returns them WITH their codes so a receipt can carry them.
	//
	// One method rather than a loop of Creates in the use case, for two
	// reasons that both belong to the database:
	//
	//   - a code that collides with an existing one has to be redrawn, and the
	//     only place that collision is visible is the unique index;
	//   - issuing must be idempotent. A redelivered webhook settles the same
	//     order twice, and the second settlement must not mint a second set of
	//     tickets. The repository decides that by what is already stored, not
	//     by a flag the caller passes.
	//
	// It returns the admissions that were CREATED by this call. A second call
	// for the same order returns none, which is how a caller tells "issued" from
	// "already issued" without asking a second question.
	IssueForOrder(ctx context.Context, order OrderLines) ([]Admission, error)

	// FindByCode resolves a scanned or typed code.
	//
	// Takes the parsed Code so an unverified string cannot reach a query, and
	// looks up by the blind index: the stored code is ciphertext and is never
	// compared directly.
	FindByCode(ctx context.Context, code Code) (*Admission, error)

	// Admit spends an admission and reports whether THIS call was the one that
	// spent it.
	//
	// The single most important method in this package. It must be one
	// conditional UPDATE — status moved to admitted only WHERE it is still
	// issued — so that two doors scanning the same ticket in the same instant
	// result in exactly one admission and one refusal. A read-then-write in
	// the use case would let both through, and at a turnstile that is two
	// people through one ticket.
	//
	// false with a nil error means the row was already spent; the caller
	// re-reads to find out whether it was admitted or voided, and by whom.
	Admit(ctx context.Context, id, by string) (bool, error)

	// VoidForOrder withdraws every admission of an order, and reports how many
	// it changed. Idempotent: a second call changes nothing and returns zero.
	VoidForOrder(ctx context.Context, orderID string) (int, error)

	// ListByOrder is the holder's own tickets, codes included, so a buyer can
	// see the QR again after losing the email.
	ListByOrder(ctx context.Context, orderID string) ([]Admission, error)

	// CountAdmitted is how many people have come in, per event, for the door's
	// own counter. Cheap enough to poll.
	CountAdmitted(ctx context.Context, eventID string) (issued int, admitted int, err error)
}

// OrderLines is what issuing needs to know about a paid order: which event,
// and how many of which tier.
//
// A value of its own rather than the order entity, so this package does not
// depend on domain/order. An admission is downstream of a sale and should not
// be able to reach back into one.
type OrderLines struct {
	OrderID string
	EventID string
	Lines   []Line
}

// Line is one tier on the order and how many admissions it owes.
type Line struct {
	TicketID    string
	TicketTitle string
	Quantity    int
	// SeatID and Seat name the reserved chair this line is for, and are empty
	// for a counted line.
	//
	// A seated line always has Quantity 1: one order line per chair, so one
	// admission per chair, which is what lets a door say "Plateia A, fila K,
	// assento 12" to the person in front of it instead of "one of three".
	//
	// seating.Label rather than three strings of this package's own: the door,
	// the wallet, the receipt and the email all have to agree about a chair
	// somebody is standing next to.
	SeatID string
	Seat   seating.Label
}

// Total is how many admissions these lines owe altogether.
func (o OrderLines) Total() int {
	total := 0
	for _, line := range o.Lines {
		if line.Quantity > 0 {
			total += line.Quantity
		}
	}
	return total
}

// CodeRenderer turns a code into a scannable image.
//
// A port rather than a direct dependency, for the usual reason and one
// specific one: rendering is the only part of issuing a ticket that can fail
// for a reason nobody can fix at runtime — a library that cannot encode — and
// a caller that holds an interface can carry on without the image. A receipt
// with a printed code and no QR is a worse ticket; a receipt that was never
// sent is not a ticket at all.
type CodeRenderer interface {
	PNG(code Code) ([]byte, error)
}
