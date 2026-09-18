// Package ticket is the application layer of the box office: it orchestrates
// the ticket entity, its persistence port and the media library, and holds no
// rule that belongs to the entity itself.
//
// Every method that names a tier by id starts with owned, and the listing is
// scoped by the caller. That check is HERE and not in the HTTP handler, for the
// same reason usecases/report says so: a rule enforced at the transport edge is
// a rule the next caller forgets, and the next caller is a CLI, a job or
// another use case that never passes through a handler. It was in the handler
// once, and the handler simply did not do it: every signed-in account could
// read, reprice and delete every tier on the platform.
package ticket

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	authdomain "vozkot/domain/auth"
	domain "vozkot/domain/ticket"
)

type Service struct {
	repository domain.Repository
	now        func() time.Time
	newID      func() string
}

// CreateInput is what an operator supplies to open a new ticket tier. OwnerID
// comes from the authenticated session, never from the request body.
type CreateInput struct {
	OwnerID string
	// EventID names the happening this tier sells admission to.
	EventID     string
	Title       string
	Description string
	PriceCents  int64
	Quantity    int
	Status      domain.Status
}

// UpdateInput replaces every operator-editable field; a partial edit is the
// caller's job to assemble, so a missing field cannot silently blank a listing.
type UpdateInput struct {
	Title       string
	Description string
	PriceCents  int64
	Quantity    int
	Status      domain.Status
}

// Page is a listing plus the total behind it, so a UI can paginate without a
// second round trip.
type Page struct {
	Items []domain.Ticket
	Total int64
}

func NewService(repository domain.Repository) *Service {
	return &Service{repository: repository, now: time.Now, newID: randomID}
}

func (s *Service) Create(ctx context.Context, input CreateInput) (*domain.Ticket, error) {
	item, err := domain.New(s.newID(), input.OwnerID, domain.Draft{
		EventID:     input.EventID,
		Title:       input.Title,
		Description: input.Description,
		PriceCents:  input.PriceCents,
		Quantity:    input.Quantity,
		Status:      input.Status,
	}, s.now())
	if err != nil {
		return nil, err
	}
	if err := s.repository.Create(ctx, item); err != nil {
		return nil, err
	}
	return item, nil
}

// Get returns one tier, refusing one that is not the caller's.
func (s *Service) Get(ctx context.Context, actor authdomain.Actor, id string) (*domain.Ticket, error) {
	return s.owned(ctx, actor, id)
}

// owned loads a tier and refuses one belonging to somebody else.
//
// The single authorisation point of this package. An operator reaches
// anything; everybody else reaches only tiers they own.
func (s *Service) owned(ctx context.Context, actor authdomain.Actor, id string) (*domain.Ticket, error) {
	item, err := s.repository.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !actor.MayReach(item.OwnerID) {
		return nil, fmt.Errorf("%w: this ticket tier belongs to another operator", authdomain.ErrForbidden)
	}
	return item, nil
}

// List returns the tiers matching a filter, with their total.
//
// No gallery hydration: artwork belongs to the EVENT now, because that is what
// it depicts. An evening selling Pista and Camarote has one poster, and a
// listing card that had to choose between two tiers' images would be choosing
// arbitrarily.
func (s *Service) List(ctx context.Context, actor authdomain.Actor, filter domain.Filter) (Page, error) {
	// Narrowed to the caller before the query runs, rather than filtered after
	// it: an unscoped listing is a full read of every box office's prices and
	// stock, and pagination would hand it over twenty rows at a time.
	if !actor.IsAdmin() {
		if !actor.Authenticated() {
			return Page{}, authdomain.ErrUnauthorized
		}
		filter.OwnerID = actor.ID
	}

	items, err := s.repository.List(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	total, err := s.repository.Count(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	return Page{Items: items, Total: total}, nil
}

func (s *Service) Update(
	ctx context.Context,
	actor authdomain.Actor,
	id string,
	input UpdateInput,
) (*domain.Ticket, error) {
	item, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if err := item.Apply(domain.Draft{
		// The tier's existing event, not one from the request. Moving a tier
		// between events would move seats somebody already bought, and the
		// orders against it would still name the old one.
		EventID:     item.EventID,
		Title:       input.Title,
		Description: input.Description,
		PriceCents:  input.PriceCents,
		Quantity:    input.Quantity,
		Status:      input.Status,
	}, s.now()); err != nil {
		return nil, err
	}
	if err := s.repository.Update(ctx, item); err != nil {
		return nil, err
	}
	return s.owned(ctx, actor, id)
}

func (s *Service) ChangeStatus(
	ctx context.Context,
	actor authdomain.Actor,
	id string,
	status domain.Status,
) (*domain.Ticket, error) {
	item, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if err := item.ChangeStatus(status, s.now()); err != nil {
		return nil, err
	}
	if err := s.repository.Update(ctx, item); err != nil {
		return nil, err
	}
	return s.owned(ctx, actor, id)
}

// Delete removes a tier. Its event's artwork is untouched: the poster belongs
// to the evening, not to one of its prices.
func (s *Service) Delete(ctx context.Context, actor authdomain.Actor, id string) error {
	if _, err := s.owned(ctx, actor, id); err != nil {
		return err
	}
	return s.repository.Delete(ctx, id)
}

func randomID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "tkt_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return "tkt_" + hex.EncodeToString(buffer)
}
