package schema

import "time"

// IdempotencyKey records a client's retry token and what it produced.
//
// The primary key is (key, scope), which is what makes claiming one an atomic
// INSERT: two simultaneous retries of the same request race to insert, exactly
// one wins, and the loser is told the work is already in flight instead of
// starting a second checkout.
type IdempotencyKey struct {
	Key   string `gorm:"primaryKey;type:varchar(255)"`
	Scope string `gorm:"primaryKey;type:varchar(64)"`

	// RequestHash catches the client that reuses a key with a different body.
	RequestHash string `gorm:"not null;type:varchar(64)"`
	State       string `gorm:"not null;type:varchar(16);default:'processing'"`
	StatusCode  int    `gorm:"not null;default:0"`
	// Response is raw bytes, not jsonb, on purpose: jsonb normalises key order
	// and whitespace, and a replay has to return byte for byte what the first
	// request answered — a client comparing the two must see the same body.
	Response []byte `gorm:"type:bytea"`

	CreatedAt   time.Time `gorm:"not null;autoCreateTime"`
	CompletedAt *time.Time
	// LeaseExpiresAt is when an unfinished claim may be taken over by a retry.
	//
	// Nullable on purpose. A row written before leases existed has none, and
	// "no lease" is exactly the orphan case a takeover is for — so NULL reads
	// as lapsed rather than as forever.
	LeaseExpiresAt *time.Time
	// ExpiresAt is indexed because pruning queries it and nothing else.
	ExpiresAt time.Time `gorm:"not null;index:idx_idempotency_expires_at"`
}

func (IdempotencyKey) TableName() string { return "idempotency_keys" }
