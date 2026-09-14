package user

import (
	"context"
	"errors"
)

var (
	ErrNotFound           = errors.New("user not found")
	ErrEmailAlreadyExists = errors.New("email already exists")
)

type Repository interface {
	Create(ctx context.Context, item *User) error
	FindByID(ctx context.Context, id string) (*User, error)
	FindByEmail(ctx context.Context, email string) (*User, error)
	// SaveProfile writes the identity block and nothing else.
	//
	// Narrow on purpose: a general Update would let any caller overwrite a
	// role, a token version or a disabled-at through a path meant for a buyer
	// filling in their own name.
	//
	// It returns ErrDocumentInUse when the document already belongs to another
	// account: decided by a unique index rather than by a prior SELECT, which
	// two simultaneous sign-ups would both pass.
	SaveProfile(ctx context.Context, id string, profile Profile) error
	// FindByDocument resolves an account by its document, through the blind
	// index. Used to recognise a returning buyer, never to list anybody.
	FindByDocument(ctx context.Context, document string) (*User, error)
	// SavePassword writes the hash and nothing else. Narrow for the same
	// reason SaveProfile is: a general Update reachable from a
	// "change my password" screen is a path that can also change a role.
	SavePassword(ctx context.Context, id, hash string) error
}
