package seating

import (
	"sort"
	"strings"
	"time"
)

// EventSeat is the sellable unit: one seat, one night, one price, one status.
//
// It carries its own labels rather than joining to the layout, and that is the
// single most important decision in this file. The rule is the one
// order_items already follows for TicketTitle and UnitPriceCents: the live row
// is the authority for STOCK, the snapshot is the authority for the RECORD. A
// venue that re-letters row I next season must not rewrite the ticket somebody
// is holding for last season.
type EventSeat struct {
	ID      string
	EventID string
	// TicketID is the tier whose price this seat sells at, and it is part of
	// every claim's WHERE clause. That is not a convenience: without it a
	// buyer could send a premium seat's id on a cheap tier's line and pay the
	// cheap price for it.
	TicketID string
	// LayoutSeatID is provenance. Nullable, because a layout can be archived
	// long after the night was sold and the seat must survive that.
	LayoutSeatID string
	Label        Label
	Kind         SeatKind
	Status       Status
	// OrderID is who holds or owns it. Empty when available or blocked.
	OrderID string
	// HoldExpiresAt mirrors the holding order's deadline, so the sweep that
	// reclaims lapsed holds can find seats by their own column instead of
	// joining every order.
	HoldExpiresAt *time.Time
	BlockReason   BlockReason
	// Version is a monotonic cursor for map deltas: a client polls "what
	// changed since 417" and gets only that. Global rather than per event,
	// which is the same cursor either way and one sequence instead of one per
	// event.
	Version int64
	// X and Y place the seat in the layout's coordinate space, snapshotted with
	// the labels. The buyer's map draws from these, which is what makes an arc
	// render as an arc.
	X         float64
	Y         float64
	RowOrder  int
	SeatOrder int
	UpdatedAt time.Time
}

// Held reports whether this seat is claimed by an order awaiting payment.
func (s *EventSeat) Held() bool { return s.Status == StatusHeld }

// EventSeating binds one event to one layout version: the manifest.
type EventSeating struct {
	Areas          map[string]AreaBinding
	EventID        string
	LayoutID       string
	LayoutVersion  int
	SeatCount      int
	BlockedCount   int
	MaterialisedAt time.Time
}

// AreaBinding gives a counted area its own inventory and immutable receipt name.
type AreaBinding struct {
	TicketID string `json:"ticketId"`
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

// ClaimRequest is one line of a basket asking for named seats.
//
// Scoped by event AND tier on purpose; see EventSeat.TicketID.
type ClaimRequest struct {
	EventID  string
	TicketID string
	OrderID  string
	SeatIDs  []string
	// HoldExpiresAt is the order's deadline, copied onto the seats.
	HoldExpiresAt time.Time
}

// Validate normalises a claim and refuses one that cannot be satisfied.
//
// It SORTS the seat ids, and that is a correctness requirement rather than
// tidiness: two buyers claiming {K11, K12} and {K12, K11} take row locks in
// opposite orders and deadlock against each other. domain/order.NormalizeItems
// sorts basket lines for exactly the same reason, and the discipline belongs in
// the domain where it cannot be forgotten at a call site.
//
// Duplicates are collapsed rather than rejected. A client that sent the same
// seat twice asked for that seat, and a claim of {K12, K12} whose RowsAffected
// comes back 1 against a requested 2 would fail a request that was satisfiable.
func (r *ClaimRequest) Validate() error {
	r.EventID = strings.TrimSpace(r.EventID)
	r.TicketID = strings.TrimSpace(r.TicketID)
	r.OrderID = strings.TrimSpace(r.OrderID)
	if r.EventID == "" {
		return ErrInvalidEvent
	}
	if r.TicketID == "" {
		return ErrInvalidTicket
	}
	r.SeatIDs = NormalizeSeatIDs(r.SeatIDs)
	if len(r.SeatIDs) == 0 {
		return ErrNoSeats
	}
	return nil
}

// NormalizeSeatIDs trims, drops blanks, removes duplicates and sorts.
//
// Exported because checkout needs the same normalisation on a draft line before
// it ever reaches a claim, and two implementations of "the order seats are
// locked in" is the one duplication this package must not have.
func NormalizeSeatIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if _, done := seen[trimmed]; done {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	sort.Strings(out)
	return out
}

// ClaimResult reports what a claim did.
//
// Unavailable is the field that matters to a buyer. "One of your four seats
// went" is not actionable; "K12 went, your other three are still yours" lets
// the picker grey exactly one chair and keep the rest selected, which is the
// difference between a seat picker that feels solid and one that feels broken.
type ClaimResult struct {
	Claimed     []EventSeat
	Unavailable []EventSeat
}

// OK reports whether every requested seat was taken.
func (r ClaimResult) OK() bool { return len(r.Unavailable) == 0 }

// Counts is the per-status tally of one event's seats, per tier.
//
// Used by the reconciliation sweep to prove the tier counters and the seats
// still agree. A projection that silently drifts from its source is the classic
// bug in this shape, so it gets the classic defence.
type Counts struct {
	TicketID  string
	Available int
	Held      int
	Sold      int
	Blocked   int
}

// Occupied is what the tier's sold + reserved must equal.
func (c Counts) Occupied() int { return c.Held + c.Sold }

// Total is what the tier's quantity must equal.
func (c Counts) Total() int { return c.Available + c.Held + c.Sold + c.Blocked }
