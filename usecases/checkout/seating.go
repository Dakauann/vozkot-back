package checkout

import (
	"context"
	"fmt"
	"strings"

	orderdomain "vozkot/domain/order"
	seatingdomain "vozkot/domain/seating"
	"vozkot/domain/uow"
)

// The seated half of a checkout, kept in one file.
//
// Everything here is reached only by a line that NAMES CHAIRS. A general
// admission event, a party selling Pista and two camarotes, never calls any
// of it, and that is the whole shape of the feature: seats are additive, and
// the counted path is byte for byte what it was.

// SeatsUnavailableError names the chairs a claim lost, and it exists because
// the alternative is useless to a buyer.
//
// "Not enough tickets available" is the right answer for counted stock: one
// number went down and there is nothing more to say. For seats it is the wrong
// answer twice over: the buyer picked specific chairs, and a picker told only
// that "something" went has to grey the whole selection and make them start
// again. Named seats let it grey exactly the one that moved.
type SeatsUnavailableError struct {
	// Seats are the ones that could not be taken, with whatever is known about
	// them. A seat that names no row at all comes back with an empty label,
	// which is how a caller tells "somebody took it" from "that id is wrong".
	Seats []seatingdomain.EventSeat
}

func (e *SeatsUnavailableError) Error() string {
	labels := make([]string, 0, len(e.Seats))
	for _, seat := range e.Seats {
		if label := seat.Label.String(); label != "" {
			labels = append(labels, label)
			continue
		}
		labels = append(labels, seat.ID)
	}
	return fmt.Sprintf("seats no longer available: %s", strings.Join(labels, ", "))
}

// Unwrap keeps this answerable by errors.Is for callers that only need to know
// the sale failed on inventory, which is what the HTTP layer maps to 409 and
// what every existing handler of ErrSeatsUnavailable already does.
func (e *SeatsUnavailableError) Unwrap() error { return seatingdomain.ErrSeatsUnavailable }

// IDs is the machine-readable half, for a client that wants to update a map.
func (e *SeatsUnavailableError) IDs() []string {
	ids := make([]string, 0, len(e.Seats))
	for _, seat := range e.Seats {
		ids = append(ids, seat.ID)
	}
	return ids
}

// claimSeats takes the named chairs for an order, or takes none and says which
// ones went.
//
// Returning an error on a partial claim is what rolls the surrounding
// transaction back, which is how a basket that lost one seat gives up every
// seat AND every counter it had already moved. The same property the counted
// path has, by the same mechanism.
func claimSeats(
	ctx context.Context,
	repositories uow.Repositories,
	request seatingdomain.ClaimRequest,
) ([]seatingdomain.EventSeat, error) {
	result, err := repositories.Seats().Claim(ctx, request)
	if err != nil {
		return nil, err
	}
	if !result.OK() {
		return nil, &SeatsUnavailableError{Seats: result.Unavailable}
	}
	return result.Claimed, nil
}

// expandSeatedLines turns one line naming four chairs into four lines of one.
//
// The asymmetry with counted lines is deliberate. A counted line is one row
// with a quantity; a seated line is one row per chair, each carrying that
// chair's label as a snapshot. It is what makes refunding one seat out of four,
// and issuing one admission per chair, fall out of the existing model instead
// of needing a second one.
//
// Order matters: the expanded lines come back in the claim's own order, which
// the repository returns sorted by section, row and seat. A receipt that listed
// chairs in the order a client happened to send them would read as noise.
func expandSeatedLines(
	lines []orderdomain.Item,
	seatsByTier map[string][]seatingdomain.EventSeat,
	newID func() string,
) []orderdomain.Item {
	if len(seatsByTier) == 0 {
		return lines
	}
	expanded := make([]orderdomain.Item, 0, len(lines))
	for _, line := range lines {
		seats := seatsByTier[line.TicketID]
		if len(seats) == 0 {
			expanded = append(expanded, line)
			continue
		}
		for index := range seats {
			seat := &seats[index]
			// A copy of the priced line per chair, with the quantity forced to
			// one. The unit price and the tier title are already on it and are
			// the same for every seat of the tier; the fee is quoted per unit
			// by orderdomain.New, so N lines of one and one line of N charge
			// the buyer the same money.
			perSeat := line
			perSeat.ID = newID()
			perSeat.Quantity = 1
			perSeat.SeatID = seat.ID
			perSeat.Seat = seat.Label
			perSeat.SeatKind = seat.Kind
			expanded = append(expanded, perSeat)
		}
	}
	return expanded
}

// releaseSeats gives an order's held chairs back.
//
// Paired with the tier's own Release and called from the same places: a hold
// that lapsed, an order the buyer cancelled, a payment that arrived too late to
// honour. Scoped to held inside the repository, so it can never take a seat
// away from an order that paid for it.
func releaseSeats(ctx context.Context, repositories uow.Repositories, orderID string) error {
	_, err := repositories.Seats().ReleaseForOrder(ctx, orderID)
	return err
}
