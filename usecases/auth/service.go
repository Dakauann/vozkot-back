package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode"

	domain "vozkot/domain/auth"
	"vozkot/domain/user"
)

const refreshRetryGrace = 10 * time.Second

type Service struct {
	users      user.Repository
	sessions   domain.SessionRepository
	passwords  domain.PasswordService
	tokens     domain.TokenService
	refreshTTL time.Duration
	now        func() time.Time
}

func NewService(users user.Repository, sessions domain.SessionRepository, passwords domain.PasswordService, tokens domain.TokenService, refreshTTL time.Duration) *Service {
	return &Service{users: users, sessions: sessions, passwords: passwords, tokens: tokens, refreshTTL: refreshTTL, now: time.Now}
}

func (s *Service) Register(ctx context.Context, input domain.CredentialsInput) (*domain.TokenPair, error) {
	name := strings.TrimSpace(input.Name)
	email := strings.ToLower(strings.TrimSpace(input.Email))
	if name == "" {
		return nil, errors.New("name is required")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, errors.New("email is invalid")
	}
	if !strongPassword(input.Password) {
		return nil, domain.ErrWeakPassword
	}
	if existing, _ := s.users.FindByEmail(ctx, email); existing != nil {
		return nil, user.ErrEmailAlreadyExists
	}
	hash, err := s.passwords.Hash(input.Password)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	item := &user.User{ID: newID("usr"), Name: name, Email: email, PasswordHash: hash, Role: user.RoleUser, CreatedAt: now, UpdatedAt: now}
	if err := s.users.Create(ctx, item); err != nil {
		return nil, err
	}
	return s.startSession(ctx, item, input.IPAddress, input.DeviceInfo)
}

func (s *Service) Login(ctx context.Context, input domain.CredentialsInput) (*domain.TokenPair, error) {
	item, err := s.users.FindByEmail(ctx, strings.ToLower(strings.TrimSpace(input.Email)))
	if err != nil || item.DisabledAt != nil || s.passwords.Verify(item.PasswordHash, input.Password) != nil {
		return nil, domain.ErrInvalidCredentials
	}
	return s.startSession(ctx, item, input.IPAddress, input.DeviceInfo)
}

func (s *Service) Refresh(ctx context.Context, rawRefresh, ipAddress, deviceInfo string) (*domain.TokenPair, error) {
	hash := s.tokens.HashRefreshToken(strings.TrimSpace(rawRefresh))
	session, err := s.sessions.FindByRefreshTokenHash(ctx, hash)
	if err != nil {
		previous, previousErr := s.sessions.FindByPreviousRefreshTokenHash(ctx, hash)
		if previousErr != nil {
			return nil, domain.ErrSessionNotFound
		}
		if previous.RotatedAt != nil && s.now().Sub(*previous.RotatedAt) <= refreshRetryGrace {
			// An honest client may retry after the first refresh response was lost.
			// Rotate from the session's current token once more instead of treating
			// the immediately previous token as theft.
			session = previous
			hash = previous.RefreshTokenHash
		} else {
			_ = s.sessions.Revoke(ctx, previous.ID)
			return nil, domain.ErrRefreshTokenReuse
		}
	}
	if session.RevokedAt != nil {
		return nil, domain.ErrSessionRevoked
	}
	if !session.Active(s.now()) {
		return nil, domain.ErrSessionExpired
	}
	item, err := s.users.FindByID(ctx, session.UserID)
	if err != nil {
		return nil, err
	}
	pair, err := s.tokens.Issue(item)
	if err != nil {
		return nil, err
	}
	nextRaw, nextHash, err := s.tokens.GenerateRefreshToken()
	if err != nil {
		return nil, err
	}
	rotated, err := s.sessions.Rotate(ctx, session.ID, hash, nextHash, pair.AccessJTI, s.now().UTC())
	if err != nil {
		return nil, err
	}
	if !rotated {
		return nil, domain.ErrSessionRevoked
	}
	pair.RefreshToken = nextRaw
	_ = ipAddress
	_ = deviceInfo
	return pair, nil
}

func (s *Service) Logout(ctx context.Context, claims *domain.Claims) error {
	if claims == nil {
		return domain.ErrUnauthorized
	}
	session, err := s.sessions.FindByAccessJTI(ctx, claims.UserID, claims.JTI)
	if err != nil {
		return nil
	}
	return s.sessions.Revoke(ctx, session.ID)
}

func (s *Service) Me(ctx context.Context, claims *domain.Claims) (*user.User, error) {
	if claims == nil {
		return nil, domain.ErrUnauthorized
	}
	item, err := s.users.FindByID(ctx, claims.UserID)
	if err != nil || item.DisabledAt != nil || item.TokenVersion != claims.TokenVersion {
		return nil, domain.ErrUnauthorized
	}
	return item, nil
}

// StartSessionFor opens a session for an account that has already been
// authenticated by some other means.
//
// Exported for the passwordless path, which proves identity with a code rather
// than a password and then needs exactly the same session, cookies and rotation
// the password path gets. A second implementation of "issue tokens" is how two
// sign-in routes end up with two different session lifetimes.
func (s *Service) StartSessionFor(ctx context.Context, item *user.User, ipAddress, deviceInfo string) (*domain.TokenPair, error) {
	return s.startSession(ctx, item, ipAddress, deviceInfo)
}

func (s *Service) startSession(ctx context.Context, item *user.User, ipAddress, deviceInfo string) (*domain.TokenPair, error) {
	pair, err := s.tokens.Issue(item)
	if err != nil {
		return nil, err
	}
	rawRefresh, refreshHash, err := s.tokens.GenerateRefreshToken()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	session := &domain.Session{
		ID: newID("sess"), UserID: item.ID, RefreshTokenHash: refreshHash,
		AccessJTI: pair.AccessJTI, DeviceInfo: deviceInfo, IPAddress: ipAddress,
		CreatedAt: now, ExpiresAt: now.Add(s.refreshTTL),
	}
	if err := s.sessions.Create(ctx, session); err != nil {
		return nil, err
	}
	pair.RefreshToken = rawRefresh
	return pair, nil
}

func strongPassword(value string) bool {
	if len(value) < 8 {
		return false
	}
	var upper, lower, digit bool
	for _, char := range value {
		upper = upper || unicode.IsUpper(char)
		lower = lower || unicode.IsLower(char)
		digit = digit || unicode.IsDigit(char)
	}
	return upper && lower && digit
}

func newID(prefix string) string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return prefix + "_" + time.Now().UTC().Format("20060102150405000000000")
	}
	return prefix + "_" + hex.EncodeToString(buffer)
}
