package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vozkot/domain/auth"
	"vozkot/domain/user"
)

// An unauthenticated request and an unavailable database are different facts,
// and the guard used to answer both with 401.
//
// That is a lie with teeth. The browser client treats 401 as "your token is
// stale", refreshes once, gets the same answer for the same reason, and clears
// the cookies, so a thirty-second PostgreSQL blip logs every buyer out in the
// middle of a checkout, and an on-sale loses its whole queue to a failover that
// the system was otherwise designed to survive.

type stubTokens struct {
	claims *auth.Claims
	err    error
}

func (s *stubTokens) Issue(*user.User) (*auth.TokenPair, error) { return nil, nil }
func (s *stubTokens) Verify(string) (*auth.Claims, error)       { return s.claims, s.err }
func (s *stubTokens) GenerateRefreshToken() (string, string, error) {
	return "", "", nil
}
func (s *stubTokens) HashRefreshToken(string) string { return "" }

type stubSessions struct {
	session *auth.Session
	err     error
}

func (s *stubSessions) Create(context.Context, *auth.Session) error { return nil }
func (s *stubSessions) FindByRefreshTokenHash(context.Context, string) (*auth.Session, error) {
	return nil, auth.ErrSessionNotFound
}
func (s *stubSessions) FindByPreviousRefreshTokenHash(context.Context, string) (*auth.Session, error) {
	return nil, auth.ErrSessionNotFound
}
func (s *stubSessions) FindByAccessJTI(context.Context, string, string) (*auth.Session, error) {
	return s.session, s.err
}
func (s *stubSessions) Rotate(context.Context, string, string, string, string, time.Time) (bool, error) {
	return false, nil
}
func (s *stubSessions) Revoke(context.Context, string) error { return nil }

// The one thing these doubles stand in for is a failure mode PostgreSQL will
// not produce on demand: a pool timeout in the middle of a request. Everything
// else about sessions is tested against the real database.

func guard(t *testing.T, sessions auth.SessionRepository) *httptest.ResponseRecorder {
	t.Helper()
	tokens := &stubTokens{claims: &auth.Claims{UserID: "usr_1", JTI: "jti_1"}}
	handler := NewAuth(tokens, sessions).Require(http.HandlerFunc(
		func(response http.ResponseWriter, _ *http.Request) {
			response.WriteHeader(http.StatusOK)
		}))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/orders/ord_1", nil)
	request.Header.Set("Authorization", "Bearer a-valid-looking-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestADatabaseFailureAnswers503NotUnauthorized(t *testing.T) {
	recorder := guard(t, &stubSessions{err: errors.New("dial tcp 10.0.0.4:5432: i/o timeout")})

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: a failover must not log every buyer out mid-checkout", recorder.Code)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After: a client cannot tell this is worth retrying")
	}
}

func TestAMissingSessionIsStillUnauthorized(t *testing.T) {
	// The genuine case: logged out, revoked, or a token from before a rotation.
	recorder := guard(t, &stubSessions{err: auth.ErrSessionNotFound})

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

func TestARevokedSessionIsUnauthorized(t *testing.T) {
	revoked := time.Now().Add(-time.Minute)
	recorder := guard(t, &stubSessions{session: &auth.Session{
		ID: "sess_1", ExpiresAt: time.Now().Add(time.Hour), RevokedAt: &revoked,
	}})

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a revoked session", recorder.Code)
	}
}

func TestAnExpiredSessionIsUnauthorized(t *testing.T) {
	recorder := guard(t, &stubSessions{session: &auth.Session{
		ID: "sess_1", ExpiresAt: time.Now().Add(-time.Minute),
	}})

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an expired session", recorder.Code)
	}
}

func TestALiveSessionReachesTheHandler(t *testing.T) {
	recorder := guard(t, &stubSessions{session: &auth.Session{
		ID: "sess_1", ExpiresAt: time.Now().Add(time.Hour),
	}})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestABadTokenNeverReachesTheDatabase(t *testing.T) {
	// A forged or expired token is refused on its signature alone. It matters
	// under load: an unauthenticated flood must not become one session lookup
	// per request on the primary.
	tokens := &stubTokens{err: errors.New("token is malformed")}
	sessions := &stubSessions{err: errors.New("this lookup should never happen")}
	handler := NewAuth(tokens, sessions).Require(http.HandlerFunc(
		func(response http.ResponseWriter, _ *http.Request) {
			t.Fatal("a request with a bad token reached the handler")
		}))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil)
	request.Header.Set("Authorization", "Bearer nonsense")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}
