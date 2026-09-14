package schema

import "time"

// Event is the persisted happening: one listing, one page, one map pin.
//
// The columns a buyer filters by, category, city, starts_at, status, are each
// indexed, because a catalogue page applies several of them at once and a
// listing that sequentially scans is a listing that gets slower every week it
// is in business.
type Event struct {
	ID      string `gorm:"primaryKey;type:varchar(32)"`
	OwnerID string `gorm:"not null;type:varchar(32);index:idx_events_owner_id"`

	// Slug is the public URL's readable half. Unique because it addresses one
	// event, and the use case disambiguates before it writes.
	Slug        string `gorm:"not null;type:varchar(120);uniqueIndex:idx_events_slug"`
	Name        string `gorm:"not null;type:varchar(255)"`
	Description string `gorm:"type:text;not null;default:''"`
	Category    string `gorm:"not null;type:varchar(40);default:'outros';index:idx_events_category"`

	Venue        string `gorm:"not null;type:varchar(255)"`
	Address      string `gorm:"type:varchar(255);not null;default:''"`
	Neighborhood string `gorm:"type:varchar(120);not null;default:''"`
	City         string `gorm:"not null;type:varchar(120);index:idx_events_city"`
	UF           string `gorm:"type:varchar(2);not null;default:''"`
	PostalCode   string `gorm:"type:varchar(20);not null;default:''"`

	// Latitude and Longitude are nullable together. An address no geocoder
	// recognises still describes a real event; the page shows it without a map.
	//
	// Stored as float64 rather than PostGIS geography on purpose: the only
	// question ever asked of them is "draw a pin here", and a whole extension
	// plus its operator classes is a heavy answer to that. Proximity search
	// ("events near me") is the thing that would justify PostGIS, and it does
	// not exist yet.
	Latitude  *float64 `gorm:"type:double precision"`
	Longitude *float64 `gorm:"type:double precision"`

	StartsAt time.Time `gorm:"not null;index:idx_events_starts_at"`
	EndsAt   *time.Time
	Status   string `gorm:"not null;type:varchar(32);default:'draft';index:idx_events_status"`

	CreatedAt time.Time `gorm:"not null;autoCreateTime;index:idx_events_created_at"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime"`

	Owner User `gorm:"foreignKey:OwnerID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (Event) TableName() string { return "events" }
