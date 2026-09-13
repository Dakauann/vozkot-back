package media

import "context"

// FileStorage is the object store behind every asset URL the product hands to a
// browser. Cloudflare R2 implements it in production; a directory on disk
// implements it for local development, and nothing above this line can tell the
// difference.
//
// contentType is REQUIRED to be the asset's real media type, because the stored
// value becomes the Content-Type the CDN serves. An object stored without one
// is served as application/octet-stream, which a browser downloads instead of
// rendering — the image silently turns into a file prompt.
type FileStorage interface {
	Upload(ctx context.Context, key string, data []byte, contentType string) error
	Delete(ctx context.Context, key string) error
	URL(key string) string
}

// Repository persists the metadata rows. The bytes live in FileStorage; these
// rows are what make them findable, ordered and deletable.
type Repository interface {
	Create(ctx context.Context, item *Media) error
	GetByID(ctx context.Context, id string) (*Media, error)
	ListByTicketID(ctx context.Context, ticketID string) ([]Media, error)
	ListByTicketIDs(ctx context.Context, ticketIDs []string) (map[string][]Media, error)
	CountByTicketID(ctx context.Context, ticketID string) (int, error)
	NextPosition(ctx context.Context, ticketID string) (int, error)
	Delete(ctx context.Context, id string) error
	DeleteByTicketID(ctx context.Context, ticketID string) ([]Media, error)
}

// Library is the application port the ticket use case depends on. Declaring it
// here keeps usecases/ticket free of any knowledge of usecases/media: both sides
// meet at the domain, which is the only package either is allowed to import.
type Library interface {
	Add(ctx context.Context, upload Upload) (*Media, error)
	ListByTicket(ctx context.Context, ticketID string) ([]Media, error)
	ListByTickets(ctx context.Context, ticketIDs []string) (map[string][]Media, error)
	Remove(ctx context.Context, ticketID, mediaID string) error
	RemoveAllByTicket(ctx context.Context, ticketID string) error
}
