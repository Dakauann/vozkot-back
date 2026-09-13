// Package media is the application rule set for ticket assets: what may be
// stored, how many, under which key, and what to undo when half of it fails.
package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	domain "vozkot/domain/media"
)

// Service implements domain/media.Library over a metadata repository and an
// object store.
type Service struct {
	repository domain.Repository
	storage    domain.FileStorage
	// images derives the variants and the placeholder. Nil is supported and
	// means assets are stored exactly as uploaded.
	images domain.ImageProcessor
	now    func() time.Time
	newID  func() string
}

var _ domain.Library = (*Service)(nil)

func NewService(repository domain.Repository, storage domain.FileStorage, images domain.ImageProcessor) *Service {
	return &Service{repository: repository, storage: storage, images: images, now: time.Now, newID: randomID}
}

// derive computes the variants and placeholder for an image, and nothing for a
// video.
//
// Every failure is swallowed on purpose. The placeholder is an optimisation;
// refusing an upload because a thumbnail could not be produced would trade a
// working image for none at all.
func (s *Service) derive(kind domain.Kind, data []byte) *domain.Derived {
	if s.images == nil || kind != domain.KindImage {
		return nil
	}
	derived, err := s.images.Process(data)
	if err != nil {
		log.Printf("media: deriving variants failed, storing the original alone: %v", err)
		return nil
	}
	return derived
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
	count, err := s.repository.CountByEventID(ctx, upload.EventID)
	if err != nil {
		return nil, err
	}
	if count >= domain.MaxItemsPerEvent {
		return nil, domain.ErrLimitReached
	}

	contentType := domain.NormalizeContentType(upload.ContentType, upload.FileName)
	id := s.newID()
	key := fmt.Sprintf("events/%s/%s%s", upload.EventID, id, domain.ExtensionFor(contentType))

	// The derived assets are computed BEFORE anything is stored, so an image
	// that cannot be decoded is refused without leaving bytes in the bucket.
	// A failure here is not fatal: the original is still a perfectly good
	// image, and a client that gets no placeholder simply renders without one.
	derived := s.derive(kind, upload.Data)

	if err := s.storage.Upload(ctx, key, upload.Data, contentType); err != nil {
		return nil, err
	}
	// The resized copies sit beside the original under a width suffix, which is
	// what lets the frontend's loader ask for a width and pay nothing for the
	// resize. A variant that fails to store costs that width and not the upload.
	if derived != nil {
		for _, variant := range derived.Variants {
			variantKey := fmt.Sprintf("events/%s/%s_%d%s", upload.EventID, id, variant.Width, domain.ExtensionFor("image/jpeg"))
			if err := s.storage.Upload(ctx, variantKey, variant.Data, "image/jpeg"); err != nil {
				log.Printf("media: store %dpx variant of %s: %v", variant.Width, id, err)
			}
		}
	}

	position, err := s.repository.NextPosition(ctx, upload.EventID)
	if err != nil {
		_ = s.storage.Delete(ctx, key)
		return nil, err
	}

	item := &domain.Media{
		ID:          id,
		EventID:     upload.EventID,
		Kind:        kind,
		StorageKey:  key,
		URL:         s.storage.URL(key),
		ContentType: contentType,
		SizeBytes:   int64(len(upload.Data)),
		Position:    position,
		CreatedAt:   s.now().UTC(),
	}
	if derived != nil {
		item.Width = derived.Width
		item.Height = derived.Height
		item.BlurDataURL = derived.BlurDataURL
		item.DominantColor = derived.DominantColor
	}
	if err := s.repository.Create(ctx, item); err != nil {
		_ = s.storage.Delete(ctx, key)
		return nil, err
	}
	return item, nil
}

func (s *Service) ListByEvent(ctx context.Context, eventID string) ([]domain.Media, error) {
	return s.repository.ListByEventID(ctx, eventID)
}

func (s *Service) ListByEvents(ctx context.Context, eventIDs []string) (map[string][]domain.Media, error) {
	if len(eventIDs) == 0 {
		return map[string][]domain.Media{}, nil
	}
	return s.repository.ListByEventIDs(ctx, eventIDs)
}

// Remove deletes the row first and the object afterwards.
//
// A stored object nobody references costs a fraction of a cent; a row pointing
// at bytes that are gone renders as a broken tile in every listing that shows
// it. When the two cannot both succeed, the cheap failure is the one to keep.
func (s *Service) Remove(ctx context.Context, eventID, mediaID string) error {
	item, err := s.repository.GetByID(ctx, mediaID)
	if err != nil {
		return err
	}
	// Scoping the lookup to the ticket keeps one listing's media ids from
	// addressing another's, and answers "not found" either way.
	if item.EventID != eventID {
		return domain.ErrNotFound
	}
	if err := s.repository.Delete(ctx, mediaID); err != nil {
		return err
	}
	_ = s.storage.Delete(ctx, item.StorageKey)
	return nil
}

// RemoveAllByEvent clears a ticket's gallery, used when the ticket itself goes.
func (s *Service) RemoveAllByEvent(ctx context.Context, eventID string) error {
	removed, err := s.repository.DeleteByEventID(ctx, eventID)
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
