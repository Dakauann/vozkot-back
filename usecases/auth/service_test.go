package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	domain "vozkot/domain/auth"
	"vozkot/domain/user"
	authRepository "vozkot/infra/repositories/auth"
	userRepository "vozkot/infra/repositories/user"
	"vozkot/infra/security"
	"vozkot/infra/testsupport"
)

// Runs against PostgreSQL: the unique email index and the refresh-token
// rotation are database behaviour, and the point of these tests is that the
// real adapters honour the domain contract.

func newService(t *testing.T) (*Service, *authRepository.SessionRepository, domain.TokenService) {
	t.Helper()
	db := testsupport.Database(t)
	users := userRepository.NewUserRepository(db)
	sessions := authRepository.NewSessionRepository(db)
	passwords := security.NewPasswordService(security.MinPasswordHashCost)
	tokens, err := security.NewTokenService("test-secret-with-at-least-thirty-two-characters", 15*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(users, sessions, passwords, tokens, 24*time.Hour), sessions, tokens
}

// seedPasswordAccount puts an account with a password straight into the
// database.
//
// Not through a use case, because no use case does this any more: an account is
// created by answering a sign-in code, and a password is added to it afterwards.
// What is under test below is session handling: refresh rotation, the grace
// window, revocation, which does not care how the row got there, so the row is
// written directly rather than dragged through a two-step verification flow.
func seedPasswordAccount(t *testing.T, email, password string) *user.User {
	t.Helper()
	db := testsupport.Database(t)
	// An empty password means an account with NO password, the shape a
	// sign-in code leaves behind. Hashing "" would give it a real hash and a
	// real one is a different account entirely.
	hash := ""
	if password != "" {
		hashed, err := security.NewPasswordService(security.MinPasswordHashCost).Hash(password)
		if err != nil {
			t.Fatal(err)
		}
		hash = hashed
	}
	now := time.Now().UTC()
	account := &user.User{
		ID: newID("usr"), Name: "Maria Silva", Email: email, PasswordHash: hash,
		Role: user.RoleUser, CreatedAt: now, UpdatedAt: now,
	}
	if err := userRepository.NewUserRepository(db).Create(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	return account
}

func cleanupUser(t *testing.T, email string) {
	t.Helper()
	db := testsupport.Database(t)
	t.Cleanup(func() {
		db.Exec("DELETE FROM users WHERE email = ?", email)
	})
}

func TestCredentialsLifecycle(t *testing.T) {
	ctx := context.Background()
	service, sessions, tokens := newService(t)
	email := testsupport.Unique("maria") + "@example.com"
	cleanupUser(t, email)

	seedPasswordAccount(t, email, "StrongPass1")
	created, err := service.Login(ctx, domain.CredentialsInput{
		Email: email, Password: "StrongPass1", DeviceInfo: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.AccessToken == "" || created.RefreshToken == "" {
		t.Fatal("expected both tokens")
	}

	claims, err := tokens.Verify(created.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	current, err := service.Me(ctx, claims)
	if err != nil || current.Email != email {
		t.Fatalf("unexpected current user: %#v, %v", current, err)
	}

	refreshed, err := service.Refresh(ctx, created.RefreshToken, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	// Presenting the previous token again inside the retry grace is an honest
	// client whose first response was lost, and is served again rather than
	// treated as theft. That grace is the service's documented behaviour, and
	// it rotates the session once more, so the tokens it returns are the live
	// ones from here on.
	retried, err := service.Refresh(ctx, created.RefreshToken, "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("a retry inside the grace window was refused: %v", err)
	}
	if retried.RefreshToken == refreshed.RefreshToken {
		t.Fatal("the grace retry did not rotate the refresh token")
	}

	liveClaims, err := tokens.Verify(retried.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(ctx, liveClaims); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.FindByAccessJTI(ctx, liveClaims.UserID, liveClaims.JTI); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Fatalf("expected logout to revoke the session, got %v", err)
	}
	// After revocation nothing about the session can be refreshed, grace or not.
	if _, err := service.Refresh(ctx, retried.RefreshToken, "127.0.0.1", "test"); err == nil {
		t.Fatal("a revoked session was refreshed")
	}
}

// The strength rule survived the removal of Register; it just moved to the only
// place a password is now chosen.
//
// The duplicate-address half of the old registration test is not reproduced
// here: nothing in this package inserts users any more, and the case-insensitive
// unique index it was really exercising is covered directly against Postgres in
// infra/database/repositories_integration_test.go.
func TestAFirstPasswordMustBeStrong(t *testing.T) {
	ctx := context.Background()
	service, _, _ := newService(t)
	email := testsupport.Unique("maria") + "@example.com"
	cleanupUser(t, email)

	// No password yet, exactly the account a sign-in code has just created.
	account := seedPasswordAccount(t, email, "")

	err := service.SetPassword(ctx, SetPasswordInput{UserID: account.ID, New: "weak"})
	if !errors.Is(err, domain.ErrWeakPassword) {
		t.Fatalf("expected weak password error, got %v", err)
	}

	if err := service.SetPassword(ctx, SetPasswordInput{UserID: account.ID, New: "StrongPass1"}); err != nil {
		t.Fatalf("a strong first password was refused: %v", err)
	}

	// And now that it HAS one, changing it takes the old one. A session alone
	// must not be enough to lock the owner out of their own account.
	if err := service.SetPassword(ctx, SetPasswordInput{UserID: account.ID, New: "AnotherPass1"}); !errors.Is(err, ErrCurrentPasswordRequired) {
		t.Fatalf("expected the current password to be demanded, got %v", err)
	}
}
