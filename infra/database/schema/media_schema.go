package schema

import "time"

// Media is one asset attached to an EVENT. StorageKey is the object's address
// inside the bucket and URL is what a browser fetches; both are stored because
// the second cannot be derived from the first once the public hostname changes.
type Media struct {
	ID          string `gorm:"primaryKey;type:varchar(32)"`
	EventID     string `gorm:"not null;type:varchar(32);index:idx_event_media_event_id"`
	Kind        string `gorm:"not null;type:varchar(16)"`
	StorageKey  string `gorm:"not null;type:varchar(512);uniqueIndex:idx_event_media_storage_key"`
	URL         string `gorm:"not null;type:varchar(1024)"`
	ContentType string `gorm:"not null;type:varchar(128)"`
	SizeBytes   int64  `gorm:"not null;default:0"`
	Position    int

	// Width and Height let a client reserve the right box before the bytes
	// arrive, which is what stops a grid resizing itself as it loads. Zero for
	// a video, and zero for an asset uploaded before this pipeline existed.
	Width  int `gorm:"not null;default:0"`
	Height int `gorm:"not null;default:0"`
	// BlurDataURL is a ~20px JPEG as a data URI: a few hundred bytes, served
	// inline in the HTML exactly as stored. Text, not bytea, for that reason.
	BlurDataURL string `gorm:"type:text;not null;default:''"`
	// DominantColor is the last-resort fallback, hex including the leading '#'.
	DominantColor string `gorm:"type:varchar(7);not null;default:''"`

	CreatedAt time.Time `gorm:"not null;autoCreateTime"`

	// Deleting an event takes its artwork with it, so a gallery cannot outlive
	// the listing it belonged to even if a caller forgets to clean up.
	Event Event `gorm:"foreignKey:EventID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

// TableName keeps the historical name. Renaming a live table is a lock and a
// deploy-ordering problem for no benefit; the column that moved is the one that
// mattered.
func (Media) TableName() string { return "ticket_media" }
