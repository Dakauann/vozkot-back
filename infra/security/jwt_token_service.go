package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"vozkot/domain/auth"
	"vozkot/domain/user"
)

type TokenService struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

func NewTokenService(secret string, accessTTL, refreshTTL time.Duration) (*TokenService, error) {
	if len(secret) < 32 {
		return nil, errors.New("JWT_SECRET must contain at least 32 characters")
	}
	if accessTTL <= 0 || refreshTTL <= 0 {
		return nil, errors.New("token TTLs must be positive")
	}
	return &TokenService{secret: []byte(secret), accessTTL: accessTTL, refreshTTL: refreshTTL}, nil
}

func (s *TokenService) Issue(item *user.User) (*auth.TokenPair, error) {
	if item == nil || item.ID == "" || item.DisabledAt != nil {
		return nil, auth.ErrInvalidCredentials
	}
	now := time.Now().UTC()
	jti, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	claims := jwt.MapClaims{
		"sub":   item.ID,
		"email": item.Email,
		"role":  string(item.Role),
		"typ":   "access",
		"ver":   item.TokenVersion,
		"jti":   jti,
		"iat":   now.Unix(),
		"exp":   now.Add(s.accessTTL).Unix(),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return nil, err
	}
	return &auth.TokenPair{AccessToken: token, AccessJTI: jti, User: item}, nil
}

func (s *TokenService) Verify(raw string) (*auth.Claims, error) {
	token, err := jwt.Parse(raw, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("invalid signing method")
		}
		return s.secret, nil
	})
	if err != nil || !token.Valid {
		return nil, auth.ErrUnauthorized
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || stringClaim(claims, "typ") != "access" {
		return nil, auth.ErrUnauthorized
	}
	version, _ := claims["ver"].(float64)
	result := &auth.Claims{
		UserID:       stringClaim(claims, "sub"),
		Email:        stringClaim(claims, "email"),
		Role:         stringClaim(claims, "role"),
		TokenVersion: int(version),
		JTI:          stringClaim(claims, "jti"),
	}
	if result.UserID == "" || result.JTI == "" {
		return nil, auth.ErrUnauthorized
	}
	return result, nil
}

func (s *TokenService) GenerateRefreshToken() (string, string, error) {
	raw, err := randomToken(32)
	if err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	return raw, s.HashRefreshToken(raw), nil
}

func (s *TokenService) HashRefreshToken(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

func (s *TokenService) RefreshTTL() time.Duration {
	return s.refreshTTL
}

func randomToken(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func stringClaim(claims jwt.MapClaims, key string) string {
	value, _ := claims[key].(string)
	return value
}
