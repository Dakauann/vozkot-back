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
// rendering, the image silently turns into a file prompt.
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
	ListByEventID(ctx context.Context, eventID string) ([]Media, error)
	ListByEventIDs(ctx context.Context, eventIDs []string) (map[string][]Media, error)
	CountByEventID(ctx context.Context, eventID string) (int, error)
	NextPosition(ctx context.Context, eventID string) (int, error)
	Delete(ctx context.Context, id string) error
	DeleteByEventID(ctx context.Context, eventID string) ([]Media, error)
}

// ImageProcessor derives what a page renders from an uploaded photo: the fixed
// widths it serves, a tiny inline placeholder, and a fallback colour.
//
// A port rather than a direct call so the media use case never imports an
// imaging library, and so a deployment that does not want the work can pass
// nil: the assets are then stored as uploaded, and a client falls back to
// rendering them without a placeholder.
type ImageProcessor interface {
	// Process returns the derived assets, or an error the caller may treat as
	// "store it as it came".
	Process(data []byte) (*Derived, error)
}

// Derived is what an ImageProcessor produced.
type Derived struct {
	Width         int
	Height        int
	BlurDataURL   string
	DominantColor string
	// Variants are the resized copies, smallest first, each with the width it
	// was rendered at.
	Variants []DerivedVariant
}

type DerivedVariant struct {
	Width  int
	Height int
	Data   []byte
}

// Library is the application port the event use case depends on. Declaring it
// here keeps usecases/event free of any knowledge of usecases/media: both sides
// meet at the domain, which is the only package either is allowed to import.
type Library interface {
	Add(ctx context.Context, upload Upload) (*Media, error)
	ListByEvent(ctx context.Context, eventID string) ([]Media, error)
	ListByEvents(ctx context.Context, eventIDs []string) (map[string][]Media, error)
	Remove(ctx context.Context, eventID, mediaID string) error
	RemoveAllByEvent(ctx context.Context, eventID string) error
}
