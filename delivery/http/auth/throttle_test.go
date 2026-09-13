package auth_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authHTTP "vozkot/delivery/http/auth"
	"vozkot/infra/config"
	middleware "vozkot/infra/http/middleware"
	redisCache "vozkot/infra/redis"
	authRepository "vozkot/infra/repositories/auth"
	userRepository "vozkot/infra/repositories/user"
	"vozkot/infra/security"
	"vozkot/infra/testsupport"
	authUsecase "vozkot/usecases/auth"
)

// Register, login and refresh are the only routes an anonymous caller can reach
// that cost real work.
//
// A login is a bcrypt compare at cost twelve — on the order of a quarter second
// of CPU — so an unthrottled flood is two attacks at once: credential stuffing,
// and a way to spend the whole fleet's CPU without ever holding an account.
// Free registration is the other half, because it makes accounts a renewable
// resource for anyone who wants to sit on an event's inventory.
//
// Real Redis, because the limiter is a Lua script's atomicity and a fake would
// be testing the fake.

type throttleHarness struct {
	mux   *http.ServeMux
	email string
}

func newThrottleHarness(t *testing.T, perAddress, perEmail int) *throttleHarness {
	t.Helper()
	db := testsupport.Database(t)
	cache := testsupport.Cache(t)
	limiter := redisCache.NewRateLimiter(cache)

	clients, err := middleware.NewClientIP(config.DefaultTrustedProxyCIDRs)
	if err != nil {
		t.Fatalf("NewClientIP: %v", err)
	}

	users := userRepository.NewUserRepository(db)
	sessions := authRepository.NewSessionRepository(db)
	passwords := security.NewPasswordService(security.MinPasswordHashCost)
	tokens, err := security.NewTokenService("test-secret-that-is-long-enough-for-hs256", 15*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}

	email := testsupport.Unique("throttle") + "@vozkot.test"
	t.Cleanup(func() {
		db.Exec("DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE email = ?)", email)
		db.Exec("DELETE FROM users WHERE email = ?", email)
	})

	service := authUsecase.NewService(users, sessions, passwords, tokens, time.Hour)
	handler := authHTTP.NewHandler(service, authHTTP.CookieConfig{},
		middleware.NewRateLimit(limiter, testsupport.Unique("login"), perEmail, time.Minute, clients),
		clients.From)

	mux := http.NewServeMux()
	addressLimit := middleware.NewRateLimit(limiter, testsupport.Unique("auth"), perAddress, time.Minute, clients)
	handler.RegisterPublic(mux, addressLimit.Require)

	return &throttleHarness{mux: mux, email: email}
}

func (h *throttleHarness) login(t *testing.T, remoteAddr, email string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": "WrongPassword1"})
	request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = remoteAddr
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.mux.ServeHTTP(recorder, request)
	return recorder
}

func TestLoginIsThrottledPerAddress(t *testing.T) {
	h := newThrottleHarness(t, 3, 100)

	for attempt := 1; attempt <= 3; attempt++ {
		if code := h.login(t, "198.51.100.7:1234", h.email, nil).Code; code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401 while inside the budget", attempt, code)
		}
	}

	refused := h.login(t, "198.51.100.7:1234", h.email, nil)

	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: an unthrottled login is a CPU denial of service", refused.Code)
	}
	if refused.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After: a client cannot back off without being told how long")
	}
}

func TestOneAddressRunningOutDoesNotAffectAnother(t *testing.T) {
	h := newThrottleHarness(t, 2, 100)
	for attempt := 0; attempt < 3; attempt++ {
		h.login(t, "198.51.100.7:1234", h.email, nil)
	}

	other := h.login(t, "203.0.113.44:9999", h.email, nil)

	if other.Code == http.StatusTooManyRequests {
		t.Fatal("one address exhausting its budget locked out an unrelated buyer")
	}
}

// TestASpoofedForwardedHeaderCannotDodgeTheLimit ties the limit to the address
// policy, and it is the test that makes the limit worth having.
//
// Without the trusted-proxy check, a caller names a new X-Forwarded-For on every
// request, lands in a fresh counter every time, and the limit stops anybody but
// the honest.
func TestASpoofedForwardedHeaderCannotDodgeTheLimit(t *testing.T) {
	h := newThrottleHarness(t, 3, 100)

	var lastCode int
	for attempt := 0; attempt < 6; attempt++ {
		// A different invented address each time, from a peer nobody trusts.
		lastCode = h.login(t, "198.51.100.7:1234", h.email, map[string]string{
			"X-Forwarded-For": fmt.Sprintf("203.0.113.%d", attempt+1),
		}).Code
	}

	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: a spoofed header chose a fresh rate-limit bucket on every attempt", lastCode)
	}
}

// TestLoginIsThrottledPerAccountAcrossAddresses is the botnet case. A guessing
// run spread over thousands of hosts trips no address counter, and the account
// being guessed would never know.
func TestLoginIsThrottledPerAccountAcrossAddresses(t *testing.T) {
	h := newThrottleHarness(t, 1000, 3)

	for attempt := 1; attempt <= 3; attempt++ {
		code := h.login(t, fmt.Sprintf("198.51.100.%d:1234", attempt), h.email, nil).Code
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401 while inside the budget", attempt, code)
		}
	}

	refused := h.login(t, "198.51.100.99:1234", h.email, nil)

	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: the account's own budget must hold across addresses", refused.Code)
	}
}

func TestAnotherAccountKeepsItsOwnBudget(t *testing.T) {
	h := newThrottleHarness(t, 1000, 2)
	for attempt := 0; attempt < 3; attempt++ {
		h.login(t, "198.51.100.7:1234", h.email, nil)
	}

	other := h.login(t, "198.51.100.7:1234", "someone-else@vozkot.test", nil)

	if other.Code == http.StatusTooManyRequests {
		t.Fatal("one account's guessing run locked another account out")
	}
}

func TestTheEmailBudgetIgnoresCaseAndWhitespace(t *testing.T) {
	// Or "Maria@..." and "maria@..." would be two budgets for one account,
	// which is a limit an attacker types their way around.
	h := newThrottleHarness(t, 1000, 2)
	h.login(t, "198.51.100.7:1234", h.email, nil)
	h.login(t, "198.51.100.7:1234", "  "+strings.ToUpper(h.email)+"  ", nil)

	refused := h.login(t, "198.51.100.7:1234", h.email, nil)

	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: capitalising the address bought a fresh budget", refused.Code)
	}
}

func TestRegisterIsThrottledToo(t *testing.T) {
	h := newThrottleHarness(t, 2, 100)
	post := func() int {
		body := `{"name":"Maria","email":"` + testsupport.Unique("new") + `@vozkot.test","password":"Senha12345"}`
		request := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "198.51.100.7:1234"
		recorder := httptest.NewRecorder()
		h.mux.ServeHTTP(recorder, request)
		return recorder.Code
	}
	t.Cleanup(func() {
		testsupport.Database(t).Exec("DELETE FROM users WHERE email LIKE 'new_%@vozkot.test'")
	})

	for attempt := 0; attempt < 2; attempt++ {
		if code := post(); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d was refused while inside the budget", attempt)
		}
	}

	if code := post(); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: free unlimited registration feeds the inventory-hostage attack", code)
	}
}

func TestRefreshIsThrottledToo(t *testing.T) {
	h := newThrottleHarness(t, 2, 100)
	post := func() int {
		request := httptest.NewRequest(http.MethodPost, "/auth/refresh",
			strings.NewReader(`{"refreshToken":"not-a-real-token"}`))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "198.51.100.7:1234"
		recorder := httptest.NewRecorder()
		h.mux.ServeHTTP(recorder, request)
		return recorder.Code
	}

	for attempt := 0; attempt < 2; attempt++ {
		if code := post(); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d was refused while inside the budget", attempt)
		}
	}

	if code := post(); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", code)
	}
}

// TestWithoutRedisTheRoutesStillWork: the limiter is load shedding, never a
// gate. A Redis outage must not close the box office's front door.
func TestWithoutRedisTheRoutesStillWork(t *testing.T) {
	db := testsupport.Database(t)
	users := userRepository.NewUserRepository(db)
	sessions := authRepository.NewSessionRepository(db)
	passwords := security.NewPasswordService(security.MinPasswordHashCost)
	tokens, err := security.NewTokenService("test-secret-that-is-long-enough-for-hs256", 15*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	service := authUsecase.NewService(users, sessions, passwords, tokens, time.Hour)

	// Exactly what the container builds when REDIS_URL is unset: no limiter at
	// all, on either half.
	var noLimiter *middleware.RateLimit
	handler := authHTTP.NewHandler(service, authHTTP.CookieConfig{}, noLimiter, nil)
	mux := http.NewServeMux()
	handler.RegisterPublic(mux, nil)

	body := `{"email":"nobody@vozkot.test","password":"WrongPassword1"}`
	request := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "198.51.100.7:1234"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: without Redis the route must still answer", recorder.Code)
	}
}
