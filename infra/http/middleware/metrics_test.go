package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The route label must be the route, never the request.
//
// Every distinct label value is a time series held in memory. A path label
// carrying real order ids is one series per order, forever, which is how a
// metrics endpoint takes down the process it was added to watch. These cases
// are the real paths this API serves.
func TestTheRouteLabelNeverCarriesAnIdentifier(t *testing.T) {
	cases := map[string]string{
		"/api/v1/orders/ord_dd14f9fda802adc3":                "/api/v1/orders/:id",
		"/api/v1/orders/ord_dd14f9fda802adc3/cancel":         "/api/v1/orders/:id/cancel",
		"/api/v1/events/evt_ae7f04ee423023b4/report":         "/api/v1/events/:id/report",
		"/api/v1/events/evt_ae7f04ee423023b4/attendees.csv":  "/api/v1/events/:id/attendees.csv",
		"/api/v1/refund-requests/req_77rj3vt2kouyje/approve": "/api/v1/refund-requests/:id/approve",
		"/api/v1/layouts/lay_627c6163b648b148/sections":      "/api/v1/layouts/:id/sections",
		"/api/v1/public/events/festa-de-junho":               "/api/v1/public/events/:slug",
		"/api/v1/public/events/evt_533fbb3cecf2d004/tiers":   "/api/v1/public/events/:id/tiers",

		// Collections and named actions are the label, and must survive intact
		// or the dashboard loses the ability to tell endpoints apart.
		"/api/v1/orders":           "/api/v1/orders",
		"/api/v1/checkout":         "/api/v1/checkout",
		"/api/v1/organiser/report": "/api/v1/organiser/report",
		"/auth/login":              "/auth/login",
		"/webhooks/asaas":          "/webhooks/asaas",
		"/":                        "/",
	}
	for path, want := range cases {
		if got := NormalizeRoute(path); got != want {
			t.Errorf("NormalizeRoute(%q) = %q, want %q", path, got, want)
		}
	}
}

// The unbounded ones, stated as a property rather than as examples: whatever
// the id, the label is the same.
func TestDifferentIdentifiersCollapseToOneLabel(t *testing.T) {
	first := NormalizeRoute("/api/v1/orders/ord_aaaaaaaaaaaaaaaa")
	second := NormalizeRoute("/api/v1/orders/ord_bbbbbbbbbbbbbbbb")
	if first != second {
		t.Fatalf("two orders produced two labels: %q and %q", first, second)
	}
	uuid := NormalizeRoute("/api/v1/orders/550e8400-e29b-41d4-a716-446655440000")
	if uuid != first {
		t.Errorf("a UUID produced %q, want %q", uuid, first)
	}
	numeric := NormalizeRoute("/api/v1/orders/1789696285")
	if numeric != first {
		t.Errorf("a long number produced %q, want %q", numeric, first)
	}
}

type capture struct {
	mu       sync.Mutex
	requests []string
	inFlight int
	peak     int
	elapsed  time.Duration
}

func (c *capture) IncHTTPRequests(method, path, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, method+" "+path+" "+status)
}

func (c *capture) ObserveHTTPLatency(_, _, _ string, elapsed time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.elapsed = elapsed
}

func (c *capture) IncHTTPInFlight(string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight++
	if c.inFlight > c.peak {
		c.peak = c.inFlight
	}
}

func (c *capture) DecHTTPInFlight(string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
}

func TestTheMiddlewareRecordsTheStatusTheHandlerWrote(t *testing.T) {
	recorder := &capture{}
	handler := NewMetrics(recorder).Require(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusCreated)
	}))

	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/v1/orders/ord_9f2c1d4e5a6b7c8d/confirm", nil))

	if len(recorder.requests) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(recorder.requests))
	}
	if got := recorder.requests[0]; got != "POST /api/v1/orders/:id/confirm 201" {
		t.Errorf("recorded %q, want the normalised route and the real status", got)
	}
	// In-flight has to come back to zero, or a gauge drifts upward for the
	// life of the process and every capacity reading is wrong.
	if recorder.inFlight != 0 {
		t.Errorf("in-flight settled at %d, want 0", recorder.inFlight)
	}
	if recorder.peak != 1 {
		t.Errorf("in-flight peaked at %d, want 1", recorder.peak)
	}
}

// A handler that writes a body without ever calling WriteHeader has sent a 200.
func TestAnImplicitOKIsRecordedAsTwoHundred(t *testing.T) {
	recorder := &capture{}
	handler := NewMetrics(recorder).Require(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil))

	if len(recorder.requests) != 1 || !strings.HasSuffix(recorder.requests[0], " 200") {
		t.Fatalf("recorded %v, want a 200", recorder.requests)
	}
}

// The scrape and the load balancer's health check must not become the traffic
// graph; at a few seconds apart they would dominate the request rate.
func TestHealthAndMetricsAreNotMeasured(t *testing.T) {
	recorder := &capture{}
	handler := NewMetrics(recorder).Require(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/health", "/metrics", "/media/cover.png"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if len(recorder.requests) != 0 {
		t.Fatalf("recorded %v, want nothing", recorder.requests)
	}
}

// Wrapping a ResponseWriter silently drops the interfaces it had. The attendee
// CSV streams, so losing Flush would buffer an export of any size in memory.
func TestTheWrapperKeepsFlush(t *testing.T) {
	flushed := false
	handler := NewMetrics(&capture{}).Require(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		flusher, ok := response.(http.Flusher)
		if !ok {
			t.Error("the wrapped writer is not an http.Flusher; streaming exports would buffer")
			return
		}
		flusher.Flush()
		flushed = true
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events/evt_1a2b3c4d5e/attendees.csv", nil))
	if !flushed {
		t.Error("the handler could not flush")
	}
}

// Metrics are optional everywhere, so a nil recorder must pass through rather
// than panic inside a request.
func TestANilRecorderIsATransparentPassThrough(t *testing.T) {
	var none *Metrics
	served := false
	handler := none.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil))
	if !served {
		t.Fatal("a nil Metrics did not serve the request")
	}
}
