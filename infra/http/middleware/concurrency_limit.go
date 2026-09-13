package middleware

import (
	"net/http"
	"strconv"
)

// ConcurrencyLimit is the origin's last overload boundary. A virtual waiting
// room should pace a major on-sale at the edge, but this guard makes a missing
// or misconfigured edge fail fast instead of parking an unbounded number of
// goroutines on the PostgreSQL connection pool.
type ConcurrencyLimit struct {
	slots chan struct{}
}

func NewConcurrencyLimit(limit int) *ConcurrencyLimit {
	if limit <= 0 {
		return nil
	}
	return &ConcurrencyLimit{slots: make(chan struct{}, limit)}
}

func (m *ConcurrencyLimit) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if m == nil {
			next.ServeHTTP(response, request)
			return
		}
		select {
		case m.slots <- struct{}{}:
			defer func() { <-m.slots }()
			next.ServeHTTP(response, request)
		default:
			response.Header().Set("Content-Type", "application/json")
			response.Header().Set("Retry-After", "1")
			response.Header().Set("X-Checkout-Capacity", strconv.Itoa(cap(m.slots)))
			response.WriteHeader(http.StatusServiceUnavailable)
			_, _ = response.Write([]byte(`{"error":"checkout is at capacity; retry shortly"}`))
		}
	})
}
