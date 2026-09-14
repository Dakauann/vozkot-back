package auth

import (
	"context"
	"errors"
	"strings"

	domain "vozkot/domain/auth"
	"vozkot/domain/user"
)

// Adding a password to an account that was created without one.
//
// Passwordless sign-in is the default and the recommended path, and it is
// deliberately not the ONLY one. Email delivery fails; a provider has an
// outage, a filter eats the message, somebody is on a plane, and an account
// whose only key arrives by email is an account that is unreachable exactly
// when its owner most wants in. Nielsen Norman's guidance on passwordless
// accounts says the same thing: offer a password AFTER the account exists, for
// the people who want one, rather than demanding one to create it.
//
// So a password here is a SECOND way in, never the first. It is never required,
// it is offered once the account is already real, and the account keeps working
// without one.

var (
	// ErrPasswordRequired is an empty new password.
	ErrPasswordRequired = errors.New("a password is required")
	// ErrCurrentPasswordRequired means the account already has one, and
	// changing it needs the old one.
	ErrCurrentPasswordRequired = errors.New("enter your current password to change it")
)

// SetPasswordInput adds or changes a password.
type SetPasswordInput struct {
	UserID string
	// Current is required only when the account ALREADY has a password.
	Current string
	New     string
}

// SetPassword adds a password to an account, or changes the one it has.
//
// The rule about `Current` is the security-relevant part. Setting a FIRST
// password needs only the session, which was itself obtained by proving control
// of the mailbox minutes ago; asking for a password the account does not have
// would be impossible to satisfy. CHANGING an existing one requires the old
// one, because a session is a weaker proof than a password plus a session: a
// borrowed laptop or a stolen token should not be enough to lock the owner out
// of their own account.
func (s *Service) SetPassword(ctx context.Context, input SetPasswordInput) error {
	account, err := s.users.FindByID(ctx, strings.TrimSpace(input.UserID))
	if err != nil {
		return err
	}

	if account.PasswordHash != "" {
		if strings.TrimSpace(input.Current) == "" {
			return ErrCurrentPasswordRequired
		}
		if err := s.passwords.Verify(account.PasswordHash, input.Current); err != nil {
			return domain.ErrInvalidCredentials
		}
	}

	if strings.TrimSpace(input.New) == "" {
		return ErrPasswordRequired
	}
	if !strongPassword(input.New) {
		return domain.ErrWeakPassword
	}

	hash, err := s.passwords.Hash(input.New)
	if err != nil {
		return err
	}
	return s.users.SavePassword(ctx, account.ID, hash)
}

// HasPassword reports whether an account can be signed into with one.
//
// Answered only for the AUTHENTICATED account, and never for an address
// somebody merely typed. "Does this email have a password" is "does this email
// have an account" with an extra step, and the sign-in flow is built
// specifically so that question has no answer.
func (s *Service) HasPassword(ctx context.Context, userID string) (bool, error) {
	account, err := s.users.FindByID(ctx, strings.TrimSpace(userID))
	if err != nil {
		return false, err
	}
	return account.PasswordHash != "", nil
}

// LoginWithPassword is the second way in, for accounts that chose one.
//
// It is the existing Login by another name, exported under one that says which
// of the two paths it is, a codebase with `Login` and `VerifyEmailSignIn` side
// by side reads as though only the first is really signing in.
func (s *Service) LoginWithPassword(ctx context.Context, input domain.CredentialsInput) (*domain.TokenPair, error) {
	return s.Login(ctx, input)
}

var _ = user.ErrNotFound
