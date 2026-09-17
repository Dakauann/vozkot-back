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
	// ErrForbidden is a caller acting on something that is not theirs.
	//
	// One sentinel for the whole system, wrapped by each use case with a
	// message naming what was refused. Transport maps this single error to 403
	// in one place, instead of five packages each inventing a forbidden error
	// only they recognise. See Actor in actor.go.
	ErrForbidden = errors.New("forbidden")
)
