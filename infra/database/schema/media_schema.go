package schema

import "time"

// Media is one asset attached to a ticket. StorageKey is the object's address
// inside the bucket and URL is what a browser fetches; both are stored because
// the second cannot be derived from the first once the public hostname changes.
type Media struct {
	ID          string    `gorm:"primaryKey;type:varchar(32)"`
	TicketID    string    `gorm:"not null;type:varchar(32);index:idx_ticket_media_ticket_id"`
	Kind        string    `gorm:"not null;type:varchar(16)"`
	StorageKey  string    `gorm:"not null;type:varchar(512);uniqueIndex:idx_ticket_media_storage_key"`
	URL         string    `gorm:"not null;type:varchar(1024)"`
	ContentType string    `gorm:"not null;type:varchar(128)"`
	SizeBytes   int64     `gorm:"not null;default:0"`
	Position    int       `gorm:"not null;default:0"`
	CreatedAt   time.Time `gorm:"not null;autoCreateTime"`

	// Deleting a ticket takes its rows with it, so a gallery cannot outlive the
	// listing it belonged to even if a caller forgets to clean up.
	Ticket Ticket `gorm:"foreignKey:TicketID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (Media) TableName() string { return "ticket_media" }
