package middleware

import (
	"context"
	"log"
	"net/http"
	"strconv"
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
	// clients resolves who is being limited. Nil falls back to the connecting
	// address, trusting no forwarding header.
	clients *ClientIP
}

func NewRateLimit(limiter cache.RateLimiter, scope string, limit int, window time.Duration, clients *ClientIP) *RateLimit {
	if window <= 0 {
		window = time.Minute
	}
	return &RateLimit{limiter: limiter, limit: limit, window: window, scope: scope, clients: clients}
}

// Allow counts one event against an explicit key, for a caller that knows
// something the middleware cannot see.
//
// Login needs it: limiting by address alone lets a botnet spread a guessing run
// across thousands of hosts and never trip a counter, so the account being
// guessed has to be counted too, and the account is in the request body, which
// only the handler has parsed.
func (m *RateLimit) Allow(ctx context.Context, key string) (cache.Decision, bool) {
	if m == nil || m.limiter == nil || m.limit <= 0 {
		return cache.Decision{Allowed: true}, true
	}
	decision, err := m.limiter.Allow(ctx, m.scope+":"+key, m.limit, m.window)
	if err != nil {
		log.Printf("rate limit: %v (allowing the request)", err)
		return cache.Decision{Allowed: true}, true
	}
	return decision, decision.Allowed
}

// WriteRefusal answers a caller that ran out of budget.
func (m *RateLimit) WriteRefusal(response http.ResponseWriter, decision cache.Decision) {
	response.Header().Set("Retry-After", strconv.Itoa(int(decision.RetryAfter.Seconds())+1))
	response.Header().Set("X-RateLimit-Limit", strconv.Itoa(m.limit))
	response.Header().Set("X-RateLimit-Remaining", "0")
	httpx.WriteError(response, http.StatusTooManyRequests, errTooManyRequests)
}

func (m *RateLimit) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if m == nil || m.limiter == nil || m.limit <= 0 {
			next.ServeHTTP(response, request)
			return
		}

		decision, allowed := m.Allow(request.Context(), m.callerKey(request))
		if !allowed {
			// Retry-After is what lets a well-behaved client back off instead of
			// hammering, which is the difference between a limiter that sheds
			// load and one that merely renames it.
			m.WriteRefusal(response, decision)
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
func (m *RateLimit) callerKey(request *http.Request) string {
	if claims, ok := auth.ClaimsFromContext(request.Context()); ok && claims.UserID != "" {
		return "user:" + claims.UserID
	}
	return "ip:" + m.clients.From(request)
}

var errTooManyRequests = &rateLimitError{}

type rateLimitError struct{}

func (e *rateLimitError) Error() string {
	return "too many requests; slow down and try again shortly"
}
