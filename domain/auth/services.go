package auth

import "vozkot/domain/user"

type PasswordService interface {
	Hash(plain string) (string, error)
	Verify(hash, plain string) error
}

type TokenService interface {
	Issue(item *user.User) (*TokenPair, error)
	Verify(accessToken string) (*Claims, error)
	GenerateRefreshToken() (raw string, hash string, err error)
	HashRefreshToken(raw string) string
}
