package middleware

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	metricsdomain "vozkot/domain/metrics"
)

// Metrics counts and times every request.
//
// It wraps the whole router rather than individual handlers, so a route added
// tomorrow is measured without anybody remembering to instrument it.
type Metrics struct {
	recorder metricsdomain.HTTPRecorder
	// skip are prefixes that are not worth a series. The metrics endpoint
	// measuring its own scrape is noise, and a health check every few seconds
	// from a load balancer would dominate the request rate and make the graph
	// describe the monitoring rather than the traffic.
	skip []string
}

func NewMetrics(recorder metricsdomain.HTTPRecorder) *Metrics {
	return &Metrics{recorder: recorder, skip: []string{"/health", "/metrics", "/media/"}}
}

// Require measures the request. Nil-safe, so a deployment with no metrics
// passes straight through.
func (m *Metrics) Require(next http.Handler) http.Handler {
	if m == nil || m.recorder == nil {
		return next
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		for _, prefix := range m.skip {
			if strings.HasPrefix(request.URL.Path, prefix) {
				next.ServeHTTP(response, request)
				return
			}
		}

		route := NormalizeRoute(request.URL.Path)
		method := request.Method

		m.recorder.IncHTTPInFlight(method, route)
		defer m.recorder.DecHTTPInFlight(method, route)

		recorder := &statusRecorder{ResponseWriter: response, status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(recorder, request)
		elapsed := time.Since(started)

		status := strconv.Itoa(recorder.status)
		m.recorder.ObserveHTTPLatency(method, route, status, elapsed)
		m.recorder.IncHTTPRequests(method, route, status)
	})
}

// NormalizeRoute collapses identifiers out of a path so the label is the ROUTE
// and never the request.
//
// THIS IS THE WHOLE COST CONTROL. Every distinct label value is its own time
// series kept in memory; a path label carrying real order ids means one series
// per order forever, which is how a metrics endpoint takes down the process it
// was added to watch. /api/v1/orders/ord_9f2/cancel has to read as
// /api/v1/orders/:id/cancel or it should not be recorded at all.
//
// Exported so the test can reach it, because a normaliser nobody can test is a
// cardinality bomb nobody can see.
func NormalizeRoute(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	// A slug sits under the public catalogue and is unbounded in exactly the
	// same way an id is: one series per event on sale.
	publicEvent := strings.HasPrefix(path, "/api/v1/public/events/")
	for index, segment := range segments {
		if segment == "" {
			continue
		}
		if looksLikeIdentifier(segment) {
			segments[index] = ":id"
			continue
		}
		// The segment straight after /public/events/ is the slug; anything
		// deeper is a named sub-resource like /tiers and stays as it is.
		if publicEvent && index == 5 {
			segments[index] = ":slug"
		}
	}
	return strings.Join(segments, "/")
}

// looksLikeIdentifier recognises the shapes this system actually mints.
//
// Ids here are a short type prefix and hex, ord_9f2c…, evt_…, tkt_…, ste_…, so
// the prefixed form is matched exactly rather than guessed at by length. The
// remaining cases are a UUID and a long number, which covers anything that
// arrives from somewhere else.
func looksLikeIdentifier(segment string) bool {
	if prefix, rest, found := strings.Cut(segment, "_"); found {
		if len(prefix) >= 2 && len(prefix) <= 5 && isLower(prefix) && len(rest) >= 6 && isAlphanumeric(rest) {
			return true
		}
	}
	if len(segment) >= 32 && strings.Contains(segment, "-") {
		return true
	}
	if len(segment) > 4 && isDigits(segment) {
		return true
	}
	// A file name keeps its extension, so attendees.csv stays readable, but the
	// stem is still checked: an export named for its order would otherwise slip
	// through as a literal.
	if stem, extension, found := strings.Cut(segment, "."); found && extension != "" {
		return looksLikeIdentifier(stem)
	}
	return false
}

func isLower(value string) bool {
	for _, character := range value {
		if character < 'a' || character > 'z' {
			return false
		}
	}
	return true
}

func isDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func isAlphanumeric(value string) bool {
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		default:
			return false
		}
	}
	return true
}

// statusRecorder remembers the code actually written.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.written {
		return
	}
	r.status = status
	r.written = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(payload []byte) (int, error) {
	// A handler that writes without WriteHeader has sent a 200, which is the
	// value this was initialised to.
	r.written = true
	return r.ResponseWriter.Write(payload)
}

// Flush and Hijack are forwarded because wrapping a ResponseWriter silently
// removes whatever interfaces it had. The CSV export streams and needs Flush;
// losing it would buffer an attendee list of any size in memory.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
