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
	// FindByAccessJTI answers "is this access token's session still live".
	//
	// It is the hot path, every authenticated request makes it, so an
	// implementation is free to answer it from a cache. Two consequences are
	// part of the contract rather than one adapter's quirk:
	//
	//   - It populates identity and liveness only, and never the refresh-token
	//     hashes. Nothing on this path needs them, and keeping them out of a
	//     cache keeps them in exactly one place. Use FindByRefreshTokenHash
	//     when the hashes are the point.
	//   - A revoked or rotated session stops being returned as soon as the
	//     implementation is told, which Revoke and Rotate below are. It must
	//     never outlive that by more than its own bounded staleness window.
	//
	// It must return ErrSessionNotFound, and only that, for "no such live
	// session". Any other error means the lookup itself failed, and a caller
	// has to be able to tell an unauthenticated request from an unavailable
	// database; answering a failover with "your token is bad" logs every
	// buyer out mid-checkout.
	FindByAccessJTI(ctx context.Context, userID, jti string) (*Session, error)
	Rotate(ctx context.Context, sessionID, expectedHash, nextHash, nextJTI string, rotatedAt time.Time) (bool, error)
	Revoke(ctx context.Context, sessionID string) error
}
