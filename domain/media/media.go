// Package media holds the assets attached to an EVENT: the artwork buyers see
// on a listing card and on the event page, and the short clips that sell it.
//
// Attached to the event and not to a ticket tier, because that is what they
// depict. An evening selling Pista and Camarote has one poster, not one per
// price, and a listing card showing a tier's image would have to pick one of
// them arbitrarily.
//
// The package knows nothing about HTTP, GORM or Cloudflare. It defines what a
// media item is, which formats are acceptable and how large each may be; where
// the bytes end up is the FileStorage port's problem, and infra answers it.
package media

import (
	"errors"
	"mime"
	"path"
	"strings"
	"time"
)

// Kind is the coarse family of an asset. Two families are enough for a listing:
// a still that can be rendered anywhere, and a clip that needs a player.
type Kind string

const (
	KindImage Kind = "image"
	KindVideo Kind = "video"
)

var (
	ErrNotFound        = errors.New("media not found")
	ErrUnsupportedType = errors.New("media type is not supported")
	ErrEmptyFile       = errors.New("media file is empty")
	ErrFileTooLarge    = errors.New("media file is larger than allowed")
	ErrLimitReached    = errors.New("media limit reached for this event")
)

const (
	// Deliberately different ceilings. An 8 MB photo is already an operator
	// mistake; a 40 MB teaser is ordinary. One shared limit would either reject
	// real video or wave through images nobody should be serving to a phone.
	MaxImageBytes = 10 << 20
	MaxVideoBytes = 50 << 20

	// A listing is a gallery, not a media library.
	MaxItemsPerEvent = 12
)

// supportedTypes is an allowlist, not a filter. Anything absent here is
// rejected before a byte reaches the bucket, so an object store that serves
// whatever it is given never has the chance to serve a surprise.
var supportedTypes = map[string]Kind{
	"image/jpeg":      KindImage,
	"image/png":       KindImage,
	"image/webp":      KindImage,
	"image/avif":      KindImage,
	"image/gif":       KindImage,
	"video/mp4":       KindVideo,
	"video/webm":      KindVideo,
	"video/quicktime": KindVideo,
}

// extensions pins the stored object's suffix per content type. The uploaded
// filename is never trusted for this: it arrives from a browser and decides how
// the CDN labels the object forever.
var extensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"image/avif":      ".avif",
	"image/gif":       ".gif",
	"video/mp4":       ".mp4",
	"video/webm":      ".webm",
	"video/quicktime": ".mov",
}

// Media is one asset attached to one ticket.
type Media struct {
	ID          string
	EventID     string
	Kind        Kind
	StorageKey  string
	URL         string
	ContentType string
	SizeBytes   int64
	Position    int

	// Width and Height are the stored image's real pixel dimensions.
	//
	// They exist so a client can reserve the right box BEFORE the bytes
	// arrive. Without them every image in a grid resizes the page as it loads,
	// which is the layout shift that makes a listing feel broken and that
	// Core Web Vitals measures directly. Zero for a video, and zero for an
	// asset uploaded before this pipeline existed.
	Width  int
	Height int

	// BlurDataURL is a tiny blurred stand-in, inline, for the moment between
	// layout and the real image arriving.
	//
	// A ~20px JPEG as a data URI rather than BlurHash or ThumbHash, and the
	// reason is that it needs NO JavaScript: it is an ordinary src the browser
	// decodes on first paint, present in the server-rendered HTML. A hash is
	// smaller on the wire but has to be decoded by a library in the client,
	// which costs a bundle, a hydration boundary and a frame of blank space,
	// and only wins past roughly fifty images on one page, which a card grid
	// does not reach above the fold.
	BlurDataURL string

	// DominantColor is the last fallback, for a client that cannot use the
	// blur at all. Hex, including the leading hash.
	DominantColor string

	CreatedAt time.Time
}

// Upload is the request to store one asset. Data is held in memory because the
// ceilings above are small enough to make streaming machinery not worth its
// complexity here; raise MaxVideoBytes much further and that trade flips.
type Upload struct {
	EventID     string
	FileName    string
	ContentType string
	Data        []byte
}

// NormalizeContentType strips the parameters a browser may append
// ("video/mp4; codecs=avc1") and falls back to the filename's extension when
// the client sent the generic application/octet-stream.
func NormalizeContentType(contentType, fileName string) string {
	parsed, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err == nil {
		parsed = strings.ToLower(parsed)
		if parsed != "" && parsed != "application/octet-stream" {
			return parsed
		}
	}
	if extension := strings.ToLower(path.Ext(fileName)); extension != "" {
		for candidate, known := range extensions {
			if known == extension {
				return candidate
			}
		}
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

// KindFor resolves the family of a content type, rejecting everything outside
// the allowlist.
func KindFor(contentType string) (Kind, error) {
	kind, ok := supportedTypes[contentType]
	if !ok {
		return "", ErrUnsupportedType
	}
	return kind, nil
}

// ExtensionFor returns the suffix the stored object key must carry.
func ExtensionFor(contentType string) string {
	return extensions[contentType]
}

// MaxBytesFor is the ceiling for a family.
func MaxBytesFor(kind Kind) int64 {
	if kind == KindVideo {
		return MaxVideoBytes
	}
	return MaxImageBytes
}

// Validate answers whether an upload may be stored at all. It runs before the
// object store is touched, so a rejected file costs nothing but the request.
func (u Upload) Validate() (Kind, error) {
	if len(u.Data) == 0 {
		return "", ErrEmptyFile
	}
	kind, err := KindFor(NormalizeContentType(u.ContentType, u.FileName))
	if err != nil {
		return "", err
	}
	if int64(len(u.Data)) > MaxBytesFor(kind) {
		return "", ErrFileTooLarge
	}
	return kind, nil
}

// SupportedContentTypes lists the allowlist for callers that need to advertise
// it, such as an upload control's accept attribute.
func SupportedContentTypes() []string {
	types := make([]string, 0, len(supportedTypes))
	for contentType := range supportedTypes {
		types = append(types, contentType)
	}
	return types
}
