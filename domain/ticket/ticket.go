// Package ticket is the box office: an ingresso, the admission a buyer holds
// for an event, not a support request.
//
// The word collides in English and the two share nothing else. An ingresso is
// priced, stocked, put on sale, sold out and cancelled. It has no assignee, no
// priority and no conversation thread.
package ticket

import (
	"errors"
	"strings"
	"time"

	"vozkot/domain/media"
)

// Status is where the ticket stands in its sale lifecycle.
type Status string

const (
	// StatusDraft is the only state in which a ticket is invisible to buyers.
	StatusDraft Status = "draft"
	// StatusOnSale is live and purchasable.
	StatusOnSale Status = "on_sale"
	// StatusSoldOut kept the event but ran out of stock.
	StatusSoldOut Status = "sold_out"
	// StatusCancelled ended the sale for good; stock is irrelevant afterwards.
	StatusCancelled Status = "cancelled"
)

// DefaultCurrency: the product is Brazilian and prices are quoted in reais.
const DefaultCurrency = "BRL"

var (
	ErrNotFound          = errors.New("ticket not found")
	ErrInvalidEventName  = errors.New("event name is required")
	ErrInvalidTitle      = errors.New("ticket title is required")
	ErrInvalidVenue      = errors.New("venue is required")
	ErrInvalidStartsAt   = errors.New("event start is required")
	ErrInvalidPrice      = errors.New("price cannot be negative")
	ErrInvalidQuantity   = errors.New("quantity must be greater than zero")
	ErrInvalidStatus     = errors.New("ticket status is invalid")
	ErrQuantityBelowSold = errors.New("quantity cannot be lower than the number already sold")
	ErrNoStock           = errors.New("ticket cannot go on sale without remaining stock")
	// ErrHasOrders refuses to delete a tier that money has changed hands on.
	// The orders are the record of that money, and the database RESTRICTs the
	// delete; this is that refusal, named.
	ErrHasOrders = errors.New("ticket has orders and cannot be deleted")
	// ErrNotOnSale means someone tried to buy a draft, a sold-out or a
	// cancelled tier.
	ErrNotOnSale = errors.New("ticket is not on sale")
	// ErrInsufficientStock means the request is larger than what is left after
	// sales and current holds.
	ErrInsufficientStock = errors.New("not enough tickets available")
)

// Ticket is one tier on sale for one event: Pista, Camarote, Meia-entrada.
//
// No json tags. This is the domain's own shape and the wire format is the
// delivery layer's decision; the two drift apart the moment an API needs a
// field the domain does not have, and mapping is what keeps that drift from
// reaching in here.
type Ticket struct {
	ID          string
	OwnerID     string
	EventName   string
	Title       string
	Description string
	Venue       string
	City        string
	StartsAt    time.Time
	PriceCents  int64
	Currency    string
	Quantity    int
	Sold        int
	// Reserved is stock held by orders that are waiting to be paid. It is not
	// sold and it is not available: a hold is a promise to one buyer that
	// expires if they do not pay.
	Reserved  int
	Status    Status
	Media     []media.Media
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Draft carries the operator-supplied fields of a ticket. Create and update
// take the same shape because they validate the same rules; what differs is
// only which fields the caller is allowed to leave alone.
type Draft struct {
	EventName   string
	Title       string
	Description string
	Venue       string
	City        string
	StartsAt    time.Time
	PriceCents  int64
	Quantity    int
	Status      Status
}

// New builds a ticket. It starts as a draft unless the caller asks otherwise,
// so nothing reaches buyers by accident.
func New(id, ownerID string, draft Draft, now time.Time) (*Ticket, error) {
	normalized, err := draft.normalize()
	if err != nil {
		return nil, err
	}
	if normalized.Status == "" {
		normalized.Status = StatusDraft
	}
	if !normalized.Status.Valid() {
		return nil, ErrInvalidStatus
	}

	timestamp := now.UTC()
	return &Ticket{
		ID:          id,
		OwnerID:     ownerID,
		EventName:   normalized.EventName,
		Title:       normalized.Title,
		Description: normalized.Description,
		Venue:       normalized.Venue,
		City:        normalized.City,
		StartsAt:    normalized.StartsAt.UTC(),
		PriceCents:  normalized.PriceCents,
		Currency:    DefaultCurrency,
		Quantity:    normalized.Quantity,
		Sold:        0,
		Status:      normalized.Status,
		CreatedAt:   timestamp,
		UpdatedAt:   timestamp,
	}, nil
}

// Apply replaces the operator-editable fields of an existing ticket.
//
// Sold is not among them: it is a consequence of sales, not an editable field,
// and quantity may not be pushed below it — that would promise refunds the box
// office cannot honour.
func (t *Ticket) Apply(draft Draft, now time.Time) error {
	normalized, err := draft.normalize()
	if err != nil {
		return err
	}
	if normalized.Quantity < t.Sold+t.Reserved {
		// Held stock counts here too: shrinking under it would break a promise
		// already made to a buyer who is in the middle of paying.
		return ErrQuantityBelowSold
	}
	if normalized.Status == "" {
		normalized.Status = t.Status
	}
	if !normalized.Status.Valid() {
		return ErrInvalidStatus
	}

	t.EventName = normalized.EventName
	t.Title = normalized.Title
	t.Description = normalized.Description
	t.Venue = normalized.Venue
	t.City = normalized.City
	t.StartsAt = normalized.StartsAt.UTC()
	t.PriceCents = normalized.PriceCents
	t.Quantity = normalized.Quantity
	t.Status = normalized.Status
	t.UpdatedAt = now.UTC()
	return nil
}

// ChangeStatus moves the ticket through its lifecycle.
func (t *Ticket) ChangeStatus(status Status, now time.Time) error {
	if !status.Valid() {
		return ErrInvalidStatus
	}
	// Putting a ticket on sale with nothing left to sell is the one transition
	// that would lie to a buyer, so it is the one the entity refuses.
	if status == StatusOnSale && t.Available() <= 0 {
		return ErrNoStock
	}
	t.Status = status
	t.UpdatedAt = now.UTC()
	return nil
}

// Available is what a new buyer can still take: capacity minus what is sold and
// minus what other buyers are currently holding.
//
// Counting held stock as available is how an event oversells. The hold is
// short-lived, and the sweeper returns it, but while it stands it belongs to
// someone.
func (t *Ticket) Available() int {
	remaining := t.Quantity - t.Sold - t.Reserved
	if remaining < 0 {
		return 0
	}
	return remaining
}

// CanReserve reports whether this ticket may hand out `quantity` more tickets
// right now. The check is advisory: the authoritative one is the conditional
// update in the repository, which is the only thing two concurrent buyers
// cannot both pass.
func (t *Ticket) CanReserve(quantity int) error {
	if quantity <= 0 {
		return ErrInvalidQuantity
	}
	if t.Status != StatusOnSale {
		return ErrNotOnSale
	}
	if t.Available() < quantity {
		return ErrInsufficientStock
	}
	return nil
}

// Valid reports whether a status is one this domain recognises.
func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusOnSale, StatusSoldOut, StatusCancelled:
		return true
	default:
		return false
	}
}

func (d Draft) normalize() (Draft, error) {
	d.EventName = strings.TrimSpace(d.EventName)
	d.Title = strings.TrimSpace(d.Title)
	d.Description = strings.TrimSpace(d.Description)
	d.Venue = strings.TrimSpace(d.Venue)
	d.City = strings.TrimSpace(d.City)

	if d.EventName == "" {
		return d, ErrInvalidEventName
	}
	if d.Title == "" {
		return d, ErrInvalidTitle
	}
	if d.Venue == "" {
		return d, ErrInvalidVenue
	}
	if d.StartsAt.IsZero() {
		return d, ErrInvalidStartsAt
	}
	if d.PriceCents < 0 {
		return d, ErrInvalidPrice
	}
	if d.Quantity <= 0 {
		return d, ErrInvalidQuantity
	}
	return d, nil
}
