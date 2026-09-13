// Package media is the application rule set for ticket assets: what may be
// stored, how many, under which key, and what to undo when half of it fails.
package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	domain "vozkot/domain/media"
)

// Service implements domain/media.Library over a metadata repository and an
// object store.
type Service struct {
	repository domain.Repository
	storage    domain.FileStorage
	now        func() time.Time
	newID      func() string
}

var _ domain.Library = (*Service)(nil)

func NewService(repository domain.Repository, storage domain.FileStorage) *Service {
	return &Service{repository: repository, storage: storage, now: time.Now, newID: randomID}
}

// Add validates, stores the bytes, then records the row.
//
// That order matters: a row written before the upload could point at an object
// that never arrived, and a listing would render a broken image. The reverse
// failure — bytes stored, row refused — is repaired below by deleting the
// object, so neither half is left stranded.
func (s *Service) Add(ctx context.Context, upload domain.Upload) (*domain.Media, error) {
	kind, err := upload.Validate()
	if err != nil {
		return nil, err
	}
	count, err := s.repository.CountByTicketID(ctx, upload.TicketID)
	if err != nil {
		return nil, err
	}
	if count >= domain.MaxItemsPerTicket {
		return nil, domain.ErrLimitReached
	}

	contentType := domain.NormalizeContentType(upload.ContentType, upload.FileName)
	id := s.newID()
	key := fmt.Sprintf("tickets/%s/%s%s", upload.TicketID, id, domain.ExtensionFor(contentType))

	if err := s.storage.Upload(ctx, key, upload.Data, contentType); err != nil {
		return nil, err
	}

	position, err := s.repository.NextPosition(ctx, upload.TicketID)
	if err != nil {
		_ = s.storage.Delete(ctx, key)
		return nil, err
	}

	item := &domain.Media{
		ID:          id,
		TicketID:    upload.TicketID,
		Kind:        kind,
		StorageKey:  key,
		URL:         s.storage.URL(key),
		ContentType: contentType,
		SizeBytes:   int64(len(upload.Data)),
		Position:    position,
		CreatedAt:   s.now().UTC(),
	}
	if err := s.repository.Create(ctx, item); err != nil {
		_ = s.storage.Delete(ctx, key)
		return nil, err
	}
	return item, nil
}

func (s *Service) ListByTicket(ctx context.Context, ticketID string) ([]domain.Media, error) {
	return s.repository.ListByTicketID(ctx, ticketID)
}

func (s *Service) ListByTickets(ctx context.Context, ticketIDs []string) (map[string][]domain.Media, error) {
	if len(ticketIDs) == 0 {
		return map[string][]domain.Media{}, nil
	}
	return s.repository.ListByTicketIDs(ctx, ticketIDs)
}

// Remove deletes the row first and the object afterwards.
//
// A stored object nobody references costs a fraction of a cent; a row pointing
// at bytes that are gone renders as a broken tile in every listing that shows
// it. When the two cannot both succeed, the cheap failure is the one to keep.
func (s *Service) Remove(ctx context.Context, ticketID, mediaID string) error {
	item, err := s.repository.GetByID(ctx, mediaID)
	if err != nil {
		return err
	}
	// Scoping the lookup to the ticket keeps one listing's media ids from
	// addressing another's, and answers "not found" either way.
	if item.TicketID != ticketID {
		return domain.ErrNotFound
	}
	if err := s.repository.Delete(ctx, mediaID); err != nil {
		return err
	}
	_ = s.storage.Delete(ctx, item.StorageKey)
	return nil
}

// RemoveAllByTicket clears a ticket's gallery, used when the ticket itself goes.
func (s *Service) RemoveAllByTicket(ctx context.Context, ticketID string) error {
	removed, err := s.repository.DeleteByTicketID(ctx, ticketID)
	if err != nil {
		return err
	}
	for _, item := range removed {
		_ = s.storage.Delete(ctx, item.StorageKey)
	}
	return nil
}

func randomID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "med_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return "med_" + hex.EncodeToString(buffer)
}
