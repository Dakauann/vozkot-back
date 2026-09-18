package seating

import "context"

// Repository is the seat inventory port.
//
// Everything that decides whether a seat may be taken lives behind it, and it
// lives behind it for the same reason tier stock does: the decision is a
// conditional UPDATE, and the row lock the database takes to evaluate that
// update IS the mutual exclusion. A read-then-write in a use case would let two
// buyers past, and at a turnstile that is two people in one chair.
type Repository interface {
	// Claim holds the named seats for an order. All or nothing.
	//
	// It must be ONE conditional update: status moved to held only WHERE it is
	// still available, scoped to the event AND the tier, so that two buyers
	// asking for the same chair in the same instant produce exactly one holder.
	//
	// When it cannot take every seat it takes NONE, and reports which ones were
	// gone. It does not return an error for that case: losing a seat is the
	// ordinary outcome of a busy onsale and it is control flow, not a fault.
	Claim(ctx context.Context, request ClaimRequest) (ClaimResult, error)

	// ReleaseForOrder returns an order's held seats to availability.
	//
	// Scoped to held: a sold seat is never released by a hold expiring, which
	// is what stops a lapsed-hold sweep arriving after settlement and putting a
	// paid seat back on sale.
	ReleaseForOrder(ctx context.Context, orderID string) (int, error)

	// CommitForOrder turns an order's held seats into sold.
	//
	// Runs in the transaction that marks the order paid, beside the tier's own
	// Commit. Returns how many it moved so a caller can tell a settlement from
	// a redelivered one without asking a second question.
	CommitForOrder(ctx context.Context, orderID string) (int, error)

	// ReleaseSoldForOrder puts an order's SOLD seats back on sale.
	//
	// The refund path, and only it. Scoped to sold for the mirror of the reason
	// ReleaseForOrder is scoped to held: a refund must return the chair, and
	// nothing else in the system may. The tier's own ReleaseSold has the same
	// shape and the same justification: refunding an order that never
	// completed would credit inventory that was already released.
	ReleaseSoldForOrder(ctx context.Context, orderID string) (int, error)

	// ListByOrder is what a receipt, a wallet and the door read.
	ListByOrder(ctx context.Context, orderID string) ([]EventSeat, error)

	// ListByEvent is the map. Status only changes; the geometry is served from
	// the layout and cached hard, because it cannot change for a published one.
	//
	// sinceVersion of 0 returns every seat; anything higher returns only what
	// has changed, which is what makes polling a busy onsale cheap.
	ListByEvent(ctx context.Context, eventID string, sinceVersion int64) ([]EventSeat, error)

	// CountsByEvent tallies each tier's seats by status, for reconciliation.
	CountsByEvent(ctx context.Context, eventID string) ([]Counts, error)

	// Materialise lays out an event's sellable seats from a published layout,
	// assigning every seat of a section to the tier named for it.
	//
	// Idempotent by refusal rather than by merge: an event that already has a
	// map gets ErrAlreadyMaterialised, because the alternative is quietly
	// duplicating every chair or quietly diverging from the first run.
	Materialise(ctx context.Context, plan MaterialisePlan) (EventSeating, error)

	// SeatingOf reports the manifest an event is selling, if any.
	SeatingOf(ctx context.Context, eventID string) (*EventSeating, error)

	// BindAreas assigns dedicated counted inventory to each area of the frozen plan.
	BindAreas(ctx context.Context, eventID string, tickets map[string]string) error

	// Block and Unblock withhold seats from sale and put them back. Neither
	// touches a held or sold seat: an organiser blocking a broken chair must
	// not silently cancel somebody's ticket, and the refusal is visible.
	Block(ctx context.Context, eventID string, seatIDs []string, reason BlockReason) (int, error)
	Unblock(ctx context.Context, eventID string, seatIDs []string) (int, error)
}

// MaterialisePlan says which layout an event sells and what each section costs.
//
// The tier-per-section map is the join between geometry and money. It is a map
// rather than a field on the section because the SAME layout is sold at
// different prices on different nights: a Tuesday matinee and a Saturday night
// share the room and not the price list.
type MaterialisePlan struct {
	EventID  string
	LayoutID string
	// TicketByCategory maps a price band to the tier its seats sell at.
	//
	// A band rather than a section, because where a seat is and what it costs
	// change on different clocks: the room is fixed for years and the price
	// list changes every night. A band is a NAME, see Category, so two wings
	// of a plateia can share one price, and the front three rows can carry
	// their own without the room being redrawn to express it.
	//
	// A band missing from the map is not sold at all and its seats are not
	// materialised, which is how an organiser closes the balcony for a night.
	TicketByCategory map[string]string
}

// LayoutRepository is the definition side: venues, layouts, sections, seats.
//
// Separate from Repository because the two have entirely different shapes of
// traffic. A layout is written by one organiser in an editor and read rarely;
// seat status is written by every buyer and read on every map poll. Binding
// them into one interface would put an editor's concerns in the transaction
// that takes money.
type LayoutRepository interface {
	CreateVenue(ctx context.Context, venue *Venue) error
	VenueByID(ctx context.Context, id string) (*Venue, error)
	ListVenues(ctx context.Context, ownerID string, limit, offset int) ([]Venue, int64, error)

	CreateLayout(ctx context.Context, layout *Layout) error
	LayoutByID(ctx context.Context, id string) (*Layout, error)
	ListLayouts(ctx context.Context, venueID string) ([]Layout, error)
	PublishLayout(ctx context.Context, id string) error
	// FreezeLayout marks a layout uneditable because an event has sold from it.
	FreezeLayout(ctx context.Context, id string) error

	// ReplaceSections writes a layout's sections and seats in one transaction.
	//
	// Replace rather than patch: the editor generates whole sections from a row
	// form, and a partial write that left half a section behind would be a
	// layout nobody could reason about. Refused outright on a frozen layout.
	ReplaceSections(ctx context.Context, layoutID string, sections []Section, seats []Seat) error
	SectionsOf(ctx context.Context, layoutID string) ([]Section, error)
	SeatsOf(ctx context.Context, layoutID string) ([]Seat, error)
}
