package user

import "time"

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	// PasswordHash is empty for every account created by a sign-in code, which
	// is every account created since passwordless sign-in landed.
	PasswordHash string     `json:"-"`
	Role         Role       `json:"role"`
	TokenVersion int        `json:"-"`
	DisabledAt   *time.Time `json:"-"`
	// Profile is the legally required identity. Every field in it is encrypted
	// at rest and none of it is serialised here: `json:"-"` because a document
	// number has no business travelling in a user object that is returned from
	// a dozen endpoints. The one endpoint that may show it says so explicitly.
	Profile   Profile   `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
