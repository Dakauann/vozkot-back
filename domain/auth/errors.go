package auth

import "errors"

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrWeakPassword       = errors.New("password does not meet security requirements")
	ErrSessionNotFound    = errors.New("session not found")
	ErrSessionRevoked     = errors.New("session has been revoked")
	ErrSessionExpired     = errors.New("session has expired")
	ErrRefreshTokenReuse  = errors.New("refresh token reuse detected")
	ErrUnauthorized       = errors.New("authentication required")
)
