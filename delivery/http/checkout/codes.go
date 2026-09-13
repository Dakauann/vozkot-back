package checkout

import (
	"errors"

	orderdomain "vozkot/domain/order"
	ticketdomain "vozkot/domain/ticket"
)

// Machine-readable codes for the refusals a buyer can actually act on.
//
// The message beside them is written for a person and is in one language; a
// client that has to branch on behaviour cannot match on it without breaking
// the first time the wording is improved or a second locale is added. These
// are the stable half of the answer.
//
// Only refusals the buyer can DO something about get a code. "Not enough
// tickets" is one of them; an internal failure is not, because there is no
// different screen to show for it.
const (
	// CodeTooManyOpenOrders: the account is holding as much unpaid inventory as
	// it is allowed to. The way out is to pay for one of those orders or give
	// it up, so the client is expected to show them and offer exactly that.
	CodeTooManyOpenOrders = "too_many_open_orders"
	// CodeTooManyHeldTickets: same, for one tier.
	CodeTooManyHeldTickets = "too_many_held_tickets"
	// CodeInsufficientStock: somebody else took them first.
	CodeInsufficientStock = "insufficient_stock"
	// CodeNotOnSale: the tier is a draft, cancelled, or its sale has ended.
	CodeNotOnSale = "not_on_sale"
	// CodeMultipleEvents: a basket reaching across two nights.
	CodeMultipleEvents = "multiple_events"
	// CodeHoldExpired: the reservation lapsed before the buyer finished.
	CodeHoldExpired = "hold_expired"
	// CodeQuantityNotAllowed: over the per-order cap, or under one ticket.
	CodeQuantityNotAllowed = "quantity_not_allowed"
)

// CodeFor names what went wrong, or "" when there is nothing useful to say.
func CodeFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, orderdomain.ErrTooManyOpenOrders):
		return CodeTooManyOpenOrders
	case errors.Is(err, orderdomain.ErrTooManyHeldTickets):
		return CodeTooManyHeldTickets
	case errors.Is(err, ticketdomain.ErrInsufficientStock):
		return CodeInsufficientStock
	case errors.Is(err, ticketdomain.ErrNotOnSale):
		return CodeNotOnSale
	case errors.Is(err, orderdomain.ErrMultipleEvents):
		return CodeMultipleEvents
	case errors.Is(err, orderdomain.ErrHoldExpired):
		return CodeHoldExpired
	case errors.Is(err, orderdomain.ErrInvalidQuantity), errors.Is(err, orderdomain.ErrTooManyItems):
		return CodeQuantityNotAllowed
	default:
		return ""
	}
}
