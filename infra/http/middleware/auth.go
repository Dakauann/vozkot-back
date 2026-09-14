package middleware

import (
	"errors"
	"log"
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
		switch {
		case errors.Is(err, auth.ErrSessionNotFound):
			// The session is genuinely gone: logged out, revoked, or a token
			// from before a rotation.
			httpx.WriteError(response, http.StatusUnauthorized, auth.ErrUnauthorized)
			return
		case err != nil:
			// The lookup itself failed: a pool timeout, a failover, an
			// unreachable database. Answering 401 here would be a lie with
			// teeth: the browser client refreshes once, that fails the same
			// way, and it clears the cookies, so a thirty-second blip logs
			// every buyer out in the middle of a checkout. 503 says what is
			// actually true and tells a client to come back.
			log.Printf("auth: session lookup failed: %v", err)
			response.Header().Set("Retry-After", "1")
			httpx.WriteError(response, http.StatusServiceUnavailable, errAuthUnavailable)
			return
		case !session.Active(time.Now()):
			httpx.WriteError(response, http.StatusUnauthorized, auth.ErrUnauthorized)
			return
		}

		next.ServeHTTP(response, request.WithContext(auth.WithClaims(request.Context(), claims)))
	})
}

// errAuthUnavailable never mentions the token: the caller's credentials were
// never judged, so saying anything about them would be wrong.
var errAuthUnavailable = errors.New("sign-in is temporarily unavailable; please try again shortly")

func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}
