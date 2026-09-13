package middleware

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vozkot/delivery/http/httpx"
	"vozkot/domain/auth"
	"vozkot/domain/cache"
)

// RateLimit caps how often one caller may hit an expensive endpoint.
//
// Checkout is the endpoint that needs it: it reserves inventory, and a script
// that calls it in a loop can hold an entire event hostage for the length of a
// hold window without paying for a single ticket. The limiter is what turns
// that from an outage into a 429.
//
// It fails OPEN. A Redis outage must not close the box office; the ceiling
// disappears, the log says so, and tickets keep selling.
type RateLimit struct {
	limiter cache.RateLimiter
	limit   int
	window  time.Duration
	// scope namespaces the counter so two endpoints do not share a budget.
	scope string
}

func NewRateLimit(limiter cache.RateLimiter, scope string, limit int, window time.Duration) *RateLimit {
	if window <= 0 {
		window = time.Minute
	}
	return &RateLimit{limiter: limiter, limit: limit, window: window, scope: scope}
}

func (m *RateLimit) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if m == nil || m.limiter == nil || m.limit <= 0 {
			next.ServeHTTP(response, request)
			return
		}

		decision, err := m.limiter.Allow(request.Context(), m.scope+":"+callerKey(request), m.limit, m.window)
		if err != nil {
			log.Printf("rate limit: %v (allowing the request)", err)
			next.ServeHTTP(response, request)
			return
		}
		if !decision.Allowed {
			// Retry-After is what lets a well-behaved client back off instead of
			// hammering, which is the difference between a limiter that sheds
			// load and one that merely renames it.
			response.Header().Set("Retry-After", strconv.Itoa(int(decision.RetryAfter.Seconds())+1))
			response.Header().Set("X-RateLimit-Limit", strconv.Itoa(m.limit))
			response.Header().Set("X-RateLimit-Remaining", "0")
			httpx.WriteError(response, http.StatusTooManyRequests, errTooManyRequests)
			return
		}

		response.Header().Set("X-RateLimit-Limit", strconv.Itoa(m.limit))
		response.Header().Set("X-RateLimit-Remaining", strconv.Itoa(decision.Remaining))
		next.ServeHTTP(response, request)
	})
}

// callerKey identifies who is being limited: the account when there is one,
// the client address otherwise.
//
// The account comes first on purpose. Limiting by address alone punishes
// everyone behind one corporate NAT and lets one account with a dozen proxies
// through.
func callerKey(request *http.Request) string {
	if claims, ok := auth.ClaimsFromContext(request.Context()); ok && claims.UserID != "" {
		return "user:" + claims.UserID
	}
	return "ip:" + clientIP(request)
}

func clientIP(request *http.Request) string {
	// Only the first hop of X-Forwarded-For is meaningful, and only behind a
	// proxy that sets it; the rest is client-controlled text.
	if forwarded := request.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, found := strings.Cut(forwarded, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(forwarded)
	}
	if real := strings.TrimSpace(request.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

var errTooManyRequests = &rateLimitError{}

type rateLimitError struct{}

func (e *rateLimitError) Error() string {
	return "too many requests; slow down and try again shortly"
}
