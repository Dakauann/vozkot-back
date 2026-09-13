package security

import (
	"errors"

	"golang.org/x/crypto/bcrypt"

	"vozkot/domain/auth"
)

const MinPasswordHashCost = 12

type PasswordService struct {
	cost int
}

func NewPasswordService(cost int) auth.PasswordService {
	if cost < MinPasswordHashCost {
		cost = MinPasswordHashCost
	}
	return &PasswordService{cost: cost}
}

func (s *PasswordService) Hash(plain string) (string, error) {
	if plain == "" {
		return "", errors.New("password required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), s.cost)
	return string(hash), err
}

func (s *PasswordService) Verify(hash, plain string) error {
	if hash == "" || plain == "" {
		return auth.ErrInvalidCredentials
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
}
