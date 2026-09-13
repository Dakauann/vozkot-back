// Package ticket is the application layer of the box office: it orchestrates
// the ticket entity, its persistence port and the media library, and holds no
// rule that belongs to the entity itself.
package ticket

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	mediadomain "vozkot/domain/media"
	domain "vozkot/domain/ticket"
)

type Service struct {
	repository domain.Repository
	media      mediadomain.Library
	now        func() time.Time
	newID      func() string
}

// CreateInput is what an operator supplies to open a new ticket tier. OwnerID
// comes from the authenticated session, never from the request body.
type CreateInput struct {
	OwnerID     string
	EventName   string
	Title       string
	Description string
	Venue       string
	City        string
	StartsAt    time.Time
	PriceCents  int64
	Quantity    int
	Status      domain.Status
}

// UpdateInput replaces every operator-editable field; a partial edit is the
// caller's job to assemble, so a missing field cannot silently blank a listing.
type UpdateInput struct {
	EventName   string
	Title       string
	Description string
	Venue       string
	City        string
	StartsAt    time.Time
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

func NewService(repository domain.Repository, media mediadomain.Library) *Service {
	return &Service{repository: repository, media: media, now: time.Now, newID: randomID}
}

func (s *Service) Create(ctx context.Context, input CreateInput) (*domain.Ticket, error) {
	item, err := domain.New(s.newID(), input.OwnerID, domain.Draft{
		EventName:   input.EventName,
		Title:       input.Title,
		Description: input.Description,
		Venue:       input.Venue,
		City:        input.City,
		StartsAt:    input.StartsAt,
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
	item.Media = []mediadomain.Media{}
	return item, nil
}

func (s *Service) Get(ctx context.Context, id string) (*domain.Ticket, error) {
	item, err := s.repository.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	assets, err := s.media.ListByTicket(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	item.Media = assets
	return item, nil
}

// List hydrates every ticket's gallery in one extra query rather than one per
// row, because a listing of twenty tickets is the common case and twenty-one
// round trips to render it is not.
func (s *Service) List(ctx context.Context, filter domain.Filter) (Page, error) {
	items, err := s.repository.List(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	total, err := s.repository.Count(ctx, filter)
	if err != nil {
		return Page{}, err
	}
	ids := make([]string, 0, len(items))
	for index := range items {
		ids = append(ids, items[index].ID)
	}
	galleries, err := s.media.ListByTickets(ctx, ids)
	if err != nil {
		return Page{}, err
	}
	for index := range items {
		items[index].Media = galleries[items[index].ID]
	}
	return Page{Items: items, Total: total}, nil
}

func (s *Service) Update(ctx context.Context, id string, input UpdateInput) (*domain.Ticket, error) {
	item, err := s.repository.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := item.Apply(domain.Draft{
		EventName:   input.EventName,
		Title:       input.Title,
		Description: input.Description,
		Venue:       input.Venue,
		City:        input.City,
		StartsAt:    input.StartsAt,
		PriceCents:  input.PriceCents,
		Quantity:    input.Quantity,
		Status:      input.Status,
	}, s.now()); err != nil {
		return nil, err
	}
	if err := s.repository.Update(ctx, item); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Service) ChangeStatus(ctx context.Context, id string, status domain.Status) (*domain.Ticket, error) {
	item, err := s.repository.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := item.ChangeStatus(status, s.now()); err != nil {
		return nil, err
	}
	if err := s.repository.Update(ctx, item); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Delete removes the gallery before the ticket so that a failure at the storage
// step still leaves a ticket the operator can retry on, rather than an
// unreachable row and a bucket full of assets nothing points to.
func (s *Service) Delete(ctx context.Context, id string) error {
	if _, err := s.repository.GetByID(ctx, id); err != nil {
		return err
	}
	if err := s.media.RemoveAllByTicket(ctx, id); err != nil {
		return err
	}
	return s.repository.Delete(ctx, id)
}

// AttachMedia stores one image or clip against an existing ticket. The ticket
// is loaded first so an upload can never invent the listing it belongs to.
func (s *Service) AttachMedia(ctx context.Context, ticketID string, upload mediadomain.Upload) (*mediadomain.Media, error) {
	item, err := s.repository.GetByID(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	upload.TicketID = item.ID
	return s.media.Add(ctx, upload)
}

func (s *Service) RemoveMedia(ctx context.Context, ticketID, mediaID string) error {
	if _, err := s.repository.GetByID(ctx, ticketID); err != nil {
		return err
	}
	return s.media.Remove(ctx, ticketID, mediaID)
}

func randomID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "tkt_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return "tkt_" + hex.EncodeToString(buffer)
}
