package schema

import "time"

// The persisted shapes of reserved seating.
//
// Every column below is declared exactly as the table actually is once the
// migration has run. That is not pedantry: a struct that says nullable against
// a column that is not sends AutoMigrate into altering the column on every
// single boot, which is DDL in steady state and an ACCESS EXCLUSIVE lock taken
// against live checkouts, forever. The Ticket schema carries the scar.

// SeatVersionSequence is the Postgres sequence behind EventSeat.Version.
//
// Named here, beside the column it feeds, because two places need it and they
// must not disagree: the migration creates it and the repository reads
// nextval() from it on every status change.
const SeatVersionSequence = "event_seats_version_seq"

// Venue is a physical room an organiser sells more than one night in.
type Venue struct {
	ID      string `gorm:"primaryKey;type:varchar(32)"`
	OwnerID string `gorm:"not null;type:varchar(32);index:idx_venues_owner_id"`
	Name    string `gorm:"not null;type:varchar(255)"`
	// Capacity is a cached sum of the published layout. The layout is the
	// authority; this is what a listing shows without counting seats.
	Capacity  int       `gorm:"not null;default:0"`
	CreatedAt time.Time `gorm:"not null;autoCreateTime"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime"`

	Owner User `gorm:"foreignKey:OwnerID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

// VenueLayout is one arrangement of one venue, versioned.
type VenueLayout struct {
	ID      string `gorm:"primaryKey;type:varchar(32)"`
	VenueID string `gorm:"not null;type:varchar(32);index:idx_layouts_venue_id"`
	OwnerID string `gorm:"not null;type:varchar(32);index:idx_layouts_owner_id"`
	Name    string `gorm:"not null;type:varchar(255)"`
	Version int    `gorm:"not null;default:1"`
	Status  string `gorm:"not null;type:varchar(16);default:'draft'"`
	// Frozen is set the moment an event bound to this version sells a seat.
	Frozen        bool      `gorm:"not null;default:false"`
	ViewBoxWidth  int       `gorm:"not null;default:1000"`
	ViewBoxHeight int       `gorm:"not null;default:1000"`
	CreatedAt     time.Time `gorm:"not null;autoCreateTime"`
	UpdatedAt     time.Time `gorm:"not null;autoUpdateTime"`

	// RESTRICT: deleting a venue that still has layouts would take away the
	// provenance of seats somebody is holding a ticket for.
	Venue Venue `gorm:"foreignKey:VenueID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

func (VenueLayout) TableName() string { return "venue_layouts" }

// LayoutSection is a named block of a layout.
type LayoutSection struct {
	ID       string `gorm:"primaryKey;type:varchar(32)"`
	LayoutID string `gorm:"not null;type:varchar(32);index:idx_layout_sections_layout_id"`
	Name     string `gorm:"not null;type:varchar(120)"`
	Kind     string `gorm:"not null;type:varchar(16);default:'seated'"`
	// Capacity is how many a standing or booth section admits. Zero for a
	// seated section, where the seats themselves are the capacity.
	Capacity int `gorm:"not null;default:0"`
	// Shape is the polygon a client paints, as a JSON array of flattened x,y
	// pairs. jsonb and []byte, which is how jobs.payload already stores an
	// opaque document: it is read as a whole and never queried into, so a
	// Postgres array type would buy nothing and cost a driver dependency.
	// OffsetX and OffsetY are the block's centre; Width and Height size a
	// marker. All four default to zero, which is what an un-placed section had
	// before placement existed.
	OffsetX float64 `gorm:"not null;default:0"`
	OffsetY float64 `gorm:"not null;default:0"`
	Width   float64 `gorm:"not null;default:0"`
	Height  float64 `gorm:"not null;default:0"`
	Shape   []byte  `gorm:"type:jsonb"`
	// Definition is the editor form that generated this block, so a saved room
	// can be reopened and changed rather than only looked at.
	Definition   []byte `gorm:"type:jsonb"`
	DisplayOrder int    `gorm:"not null;default:0"`

	// CASCADE: a section has no meaning without its layout, and a layout that
	// still has events selling from it cannot be deleted anyway.
	Layout VenueLayout `gorm:"foreignKey:LayoutID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (LayoutSection) TableName() string { return "layout_sections" }

// LayoutSeat is one chair in a layout: the definition, reused every night.
type LayoutSeat struct {
	ID        string  `gorm:"primaryKey;type:varchar(32)"`
	SectionID string  `gorm:"not null;type:varchar(32);index:idx_layout_seats_section_id"`
	RowLabel  string  `gorm:"not null;type:varchar(16)"`
	SeatLabel string  `gorm:"not null;type:varchar(16)"`
	X         float64 `gorm:"not null;default:0"`
	Y         float64 `gorm:"not null;default:0"`
	Rotation  float64 `gorm:"not null;default:0"`
	Kind      string  `gorm:"not null;type:varchar(24);default:'standard'"`
	// RowOrder and SeatOrder are the authoritative ordering, and adjacency.
	// Labels cannot be sorted: houses skip row I, "10" sorts before "9" as
	// text, and old theatres number odd and even outward from the centre aisle.
	RowOrder  int `gorm:"not null;default:0"`
	SeatOrder int `gorm:"not null;default:0"`

	Section LayoutSection `gorm:"foreignKey:SectionID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (LayoutSeat) TableName() string { return "layout_seats" }

// EventSeating binds one event to one layout version: the manifest.
type EventSeating struct {
	EventID        string    `gorm:"primaryKey;type:varchar(32)"`
	LayoutID       string    `gorm:"not null;type:varchar(32);index:idx_event_seatings_layout_id"`
	LayoutVersion  int       `gorm:"not null;default:1"`
	SeatCount      int       `gorm:"not null;default:0"`
	BlockedCount   int       `gorm:"not null;default:0"`
	MaterialisedAt time.Time `gorm:"not null;autoCreateTime"`

	Event Event `gorm:"foreignKey:EventID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (EventSeating) TableName() string { return "event_seatings" }

// EventSeat is the sellable unit: one seat, one night, one price, one status.
//
// The label columns are denormalised on purpose, following the rule OrderItem
// already follows for TicketTitle and UnitPriceCents: the live row is the
// authority for stock, the snapshot is the authority for the record. A venue
// that re-letters row I next season must not rewrite last season's ticket.
type EventSeat struct {
	ID      string `gorm:"primaryKey;type:varchar(32)"`
	EventID string `gorm:"not null;type:varchar(32);index:idx_event_seats_event_id"`
	// TicketID is in every claim's WHERE clause. Without it a buyer could send
	// a premium seat's id on a cheap tier's line and pay the cheap price.
	//
	// RESTRICT: a tier with seats materialised against it has stock, and the
	// tier delete path already refuses tiers with orders for the same reason.
	TicketID string `gorm:"not null;type:varchar(32);index:idx_event_seats_ticket_id"`
	// LayoutSeatID is provenance and is NULLABLE: a layout may be archived long
	// after the night was sold, and the seat has to survive that.
	LayoutSeatID *string `gorm:"type:varchar(32)"`
	SectionName  string  `gorm:"not null;type:varchar(120);default:''"`
	RowLabel     string  `gorm:"not null;type:varchar(16);default:''"`
	SeatLabel    string  `gorm:"not null;type:varchar(16);default:''"`
	Kind         string  `gorm:"not null;type:varchar(24);default:'standard'"`
	Status       string  `gorm:"not null;type:varchar(16);default:'available'"`
	// X and Y are the seat's place in the layout's coordinate space, copied
	// here with the labels and for the same reason.
	//
	// Without them the buyer's map has no geometry at all, which is not a
	// cosmetic gap: a round arena renders as straight rows, and a seat map that
	// does not resemble the room cannot do the one thing a seat map is for,
	// which is letting somebody work out where they will be sitting.
	//
	// Snapshotted rather than joined because a sold seat's position has to
	// outlive the layout it came from, exactly as its name does.
	X float64 `gorm:"not null;default:0"`
	Y float64 `gorm:"not null;default:0"`
	// OrderID is who holds or owns it, NULL when nobody does.
	//
	// Nullable rather than empty-string, because the partial index below is
	// what makes "which seats does this order have" one indexed lookup instead
	// of a scan past every available chair in the house.
	OrderID       *string    `gorm:"type:varchar(32)"`
	HoldExpiresAt *time.Time `gorm:"index:idx_event_seats_hold_expires_at"`
	BlockReason   string     `gorm:"not null;type:varchar(24);default:''"`
	// Version is the monotonic map cursor, from a sequence the migration
	// creates. A client polls "what changed since 417" and gets only that.
	Version   int64     `gorm:"not null;default:0"`
	RowOrder  int       `gorm:"not null;default:0"`
	SeatOrder int       `gorm:"not null;default:0"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime"`

	Event  Event  `gorm:"foreignKey:EventID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
	Ticket Ticket `gorm:"foreignKey:TicketID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT"`
}

func (EventSeat) TableName() string { return "event_seats" }
