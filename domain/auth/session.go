package auth

import (
	"context"
	"time"
)

type Session struct {
	ID                       string     `json:"id"`
	UserID                   string     `json:"-"`
	RefreshTokenHash         string     `json:"-"`
	PreviousRefreshTokenHash string     `json:"-"`
	AccessJTI                string     `json:"-"`
	DeviceInfo               string     `json:"deviceInfo"`
	IPAddress                string     `json:"ipAddress"`
	ExpiresAt                time.Time  `json:"expiresAt"`
	CreatedAt                time.Time  `json:"createdAt"`
	RotatedAt                *time.Time `json:"-"`
	RevokedAt                *time.Time `json:"-"`
}

func (s *Session) Active(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

type SessionRepository interface {
	Create(ctx context.Context, session *Session) error
	FindByRefreshTokenHash(ctx context.Context, hash string) (*Session, error)
	FindByPreviousRefreshTokenHash(ctx context.Context, hash string) (*Session, error)
	FindByAccessJTI(ctx context.Context, userID, jti string) (*Session, error)
	Rotate(ctx context.Context, sessionID, expectedHash, nextHash, nextJTI string, rotatedAt time.Time) (bool, error)
	Revoke(ctx context.Context, sessionID string) error
}
