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

	created, err := service.Register(ctx, domain.CredentialsInput{
		Name: "Maria Silva", Email: email, Password: "StrongPass1", DeviceInfo: "test",
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
	// treated as theft. That grace is the service's documented behaviour — and
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

func TestRegistrationValidation(t *testing.T) {
	ctx := context.Background()
	service, _, _ := newService(t)
	email := testsupport.Unique("maria") + "@example.com"
	cleanupUser(t, email)

	_, err := service.Register(ctx, domain.CredentialsInput{Name: "Maria", Email: email, Password: "weak"})
	if !errors.Is(err, domain.ErrWeakPassword) {
		t.Fatalf("expected weak password error, got %v", err)
	}

	_, err = service.Register(ctx, domain.CredentialsInput{Name: "Maria", Email: email, Password: "StrongPass1"})
	if err != nil {
		t.Fatal(err)
	}
	// Case-insensitive: the unique index is on the lowercased address.
	_, err = service.Register(ctx, domain.CredentialsInput{Name: "Other", Email: "MARIA" + email[5:], Password: "StrongPass1"})
	if !errors.Is(err, user.ErrEmailAlreadyExists) {
		t.Fatalf("expected duplicate email error, got %v", err)
	}
}
