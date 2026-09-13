package middleware

import (
	"net/http"
	"strings"
	"time"

	"vozkot/delivery/http/httpx"
	"vozkot/domain/auth"
)

type Auth struct {
	tokens   auth.TokenService
	sessions auth.SessionRepository
}

func NewAuth(tokens auth.TokenService, sessions auth.SessionRepository) *Auth {
	return &Auth{tokens: tokens, sessions: sessions}
}

func (m *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		raw := bearerToken(request.Header.Get("Authorization"))
		if raw == "" {
			if cookie, err := request.Cookie("accessToken"); err == nil {
				raw = cookie.Value
			}
		}
		claims, err := m.tokens.Verify(raw)
		if err != nil {
			httpx.WriteError(response, http.StatusUnauthorized, auth.ErrUnauthorized)
			return
		}
		session, err := m.sessions.FindByAccessJTI(request.Context(), claims.UserID, claims.JTI)
		if err != nil || !session.Active(time.Now()) {
			httpx.WriteError(response, http.StatusUnauthorized, auth.ErrUnauthorized)
			return
		}
		next.ServeHTTP(response, request.WithContext(auth.WithClaims(request.Context(), claims)))
	})
}

func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}
