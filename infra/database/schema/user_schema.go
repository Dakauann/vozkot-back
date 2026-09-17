package schema

import (
	"time"

	"vozkot/infra/crypto/piigorm"
)

// Blind index scopes.
//
// One constant per protected column, and they must never be reused: the scope
// is the only thing separating "this CPF" from "this phone number" when both
// happen to be the same digits, and two columns sharing a scope would let a
// lookup on one match a row by the other.
const (
	UserDocumentBlindScope = "user.document.v1"
	UserPhoneBlindScope    = "user.phone.v1"
)

type User struct {
	ID   string `gorm:"primaryKey;type:varchar(32)"`
	Name string `gorm:"not null;type:varchar(255)"`
	// Email stays in the clear, and that is a considered exception rather than
	// an oversight. It is the account's login identifier, it is matched exactly
	// on every sign-in, it carries a unique constraint, and it is already in
	// the logs of every mail server the message passed through. Encrypting it
	// would buy a blind index and lose the ability to read the table, while the
	// fields that actually enable impersonation: document, legal name, date of
	// birth; are the ones sealed below.
	Email string `gorm:"not null;type:varchar(320);uniqueIndex:idx_users_email"`
	// PasswordHash is empty for accounts created by a sign-in code, which is
	// every account made after passwordless sign-in landed. It stays on the
	// table for the seeded operator accounts that still have one.
	PasswordHash string     `gorm:"not null;type:varchar(255);default:''"`
	Role         string     `gorm:"not null;type:varchar(32);default:'user'"`
	TokenVersion int        `gorm:"not null;default:0"`
	DisabledAt   *time.Time `gorm:"index:idx_users_disabled_at"`

	// --- the legally required identity, sealed at rest --------------------
	//
	// The type is plaintext: "this is a CPF" identifies nobody on its own and
	// is needed to know how to read the number beside it.
	DocumentType string `gorm:"type:varchar(16);not null;default:''"`
	// Document is ciphertext. DocumentBlind is its searchable companion, and
	// it is UNIQUE, which is what stops one CPF being spread across several
	// accounts to get around a per-document purchase cap.
	Document      piigorm.EncryptedString `gorm:"type:bytea"`
	DocumentBlind piigorm.BlindIndex      `gorm:"type:bytea;uniqueIndex:idx_users_document_blind"`
	LegalName     piigorm.EncryptedString `gorm:"type:bytea"`
	// BirthDate is sealed text in YYYY-MM-DD, not a date column: a date of
	// birth has no timezone, and storing it as an instant is how "born on the
	// 1st" becomes the 31st somewhere else.
	BirthDate  piigorm.EncryptedString `gorm:"type:bytea"`
	Phone      piigorm.EncryptedString `gorm:"type:bytea"`
	PhoneBlind piigorm.BlindIndex      `gorm:"type:bytea;index:idx_users_phone_blind"`
	// The optional demographics, sealed like everything else the buyer told us.
	//
	// Encrypting a city buys little on its own and costs nothing here: this copy
	// is only ever read one row at a time, by the account that owns it, to
	// re-render the form. The copy a report GROUPs BY lives on the order, in the
	// clear, frozen at purchase. Keeping the rule "everything a person told us
	// about themselves is encrypted" whole is worth more than the exception.
	Gender piigorm.EncryptedString `gorm:"type:bytea"`
	City   piigorm.EncryptedString `gorm:"type:bytea"`
	UF     piigorm.EncryptedString `gorm:"type:bytea"`
	// PhoneVerifiedAt separates a number that was typed from one that was
	// proven. Only the second is worth anything.
	PhoneVerifiedAt *time.Time
	// ProfileCompletedAt is when the identity block was first filled in. It is
	// the flag every "do we need to ask for this" check reads, so that question
	// never has to be answered by inspecting four encrypted columns.
	ProfileCompletedAt *time.Time

	CreatedAt time.Time `gorm:"not null;autoCreateTime"`
	UpdatedAt time.Time `gorm:"not null;autoUpdateTime"`
}

func (User) TableName() string { return "users" }

// VerificationChallenge is one outstanding sign-in or phone code.
//
// Nothing in this table is readable as a list of who is signing in: the
// destination is stored sealed, and the only searchable form of it is a blind
// index. The code itself is never here, only a slow hash of it.
type VerificationChallenge struct {
	ID      string `gorm:"primaryKey;type:varchar(32)"`
	Purpose string `gorm:"not null;type:varchar(32);index:idx_challenges_lookup,priority:1"`
	// DestinationBlind is what the rate limits count and what a resend finds.
	DestinationBlind piigorm.BlindIndex      `gorm:"type:bytea;not null;index:idx_challenges_lookup,priority:2"`
	Destination      piigorm.EncryptedString `gorm:"type:bytea;not null"`
	CodeHash         string                  `gorm:"not null;type:varchar(255)"`
	Attempts         int                     `gorm:"not null;default:0"`
	// UserID is set for a phone challenge, which belongs to an account that
	// already exists. Sign-in leaves it empty: the account may not exist yet.
	UserID string `gorm:"type:varchar(32);not null;default:''"`

	ExpiresAt  time.Time `gorm:"not null;index:idx_challenges_expires_at"`
	ConsumedAt *time.Time
	CreatedAt  time.Time `gorm:"not null;autoCreateTime;index:idx_challenges_created_at"`
}

func (VerificationChallenge) TableName() string { return "verification_challenges" }
