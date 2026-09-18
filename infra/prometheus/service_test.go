package prometheus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metricsdomain "vozkot/domain/metrics"
	"vozkot/infra/http/middleware"
)

// What the scrape actually renders.
//
// Asserted against the exposition text rather than against the collector
// objects, because the only thing a dashboard or an alert rule can read is this
// output. A metric that is built but never registered looks identical from
// inside the process and is invisible from outside it.
func scrape(t *testing.T, service *Service) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	service.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", recorder.Code)
	}
	return recorder.Body.String()
}

func TestTheScrapeCarriesEveryMetricWithItsReplica(t *testing.T) {
	service := New("replica-1")

	service.IncHTTPRequests(http.MethodGet, "/api/v1/orders", "200")
	service.ObserveHTTPLatency(http.MethodGet, "/api/v1/orders", "200", 120*time.Millisecond)
	service.IncJobParked("create_charge")
	service.SetQueueJobs("dead", 3)
	service.SetOrdersByStatus("refund_required", 2)
	service.IncRateLimited("auth", metricsdomain.RateLimitReasonError)

	body := scrape(t, service)
	for _, want := range []string{
		`app_http_requests_total{method="GET",path="/api/v1/orders",replica_id="replica-1",status="200"} 1`,
		`app_http_request_duration_seconds_count{method="GET",path="/api/v1/orders",replica_id="replica-1",status="200"} 1`,
		`app_queue_jobs_parked_total{replica_id="replica-1",type="create_charge"} 1`,
		`app_queue_jobs{replica_id="replica-1",status="dead"} 3`,
		`app_orders_by_status{replica_id="replica-1",status="refund_required"} 2`,
		`app_rate_limited_total{limiter="auth",reason="limiter_error",replica_id="replica-1"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape is missing:\n  %s", want)
		}
	}
}

// The runtime collectors have to carry replica_id too, or a dashboard filtered
// by replica shows host-scoped memory beside replica-scoped request rates and
// somebody debugs the wrong process during an incident.
func TestTheRuntimeCollectorsAreReplicaScoped(t *testing.T) {
	body := scrape(t, New("replica-7"))
	for _, want := range []string{
		`go_goroutines{replica_id="replica-7"}`,
		`process_resident_memory_bytes{replica_id="replica-7"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape is missing the replica-scoped runtime series:\n  %s", want)
		}
	}
}

// A quiet night must read zero, not "No data". An absent series and a healthy
// one look the same on a graph, and rate() over a counter that was never
// touched returns nothing at all, so an alert on a limiter that has not yet
// failed could not tell "fine" from "never wired up".
func TestTheQuietPathIsSeededWithZeroes(t *testing.T) {
	body := scrape(t, New("replica-1"))
	for _, want := range []string{
		`app_queue_jobs{replica_id="replica-1",status="dead"} 0`,
		`app_orders_by_status{replica_id="replica-1",status="refund_required"} 0`,
		`app_rate_limited_total{limiter="auth",reason="limiter_error",replica_id="replica-1"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape does not seed:\n  %s", want)
		}
	}
}

// No identifier reaches a label, asserted ACROSS THE SEAM.
//
// The adapter records whatever it is handed, and the middleware is what
// collapses an id into a route, so neither package can prove this alone: the
// middleware test shows the route is normalised, and this shows the normalised
// route is what ends up on the scrape. One series per order is unbounded
// memory, and it would be introduced by wiring, not by either half.
func TestNoIdentifierSurvivesFromRequestToScrape(t *testing.T) {
	service := New("replica-1")
	handler := middleware.NewMetrics(service).Require(http.HandlerFunc(
		func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusOK) },
	))

	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/api/v1/orders/ord_dd14f9fda802adc3", nil))

	body := scrape(t, service)
	if strings.Contains(body, "ord_dd14f9fda802adc3") {
		t.Error("an order id reached the scrape output: one time series per order is unbounded memory")
	}
	if !strings.Contains(body, `path="/api/v1/orders/:id"`) {
		t.Error("the normalised route is not on the scrape; the middleware and the adapter are not wired together")
	}
}

// Every recorder is optional at its call site.
func TestANilServiceRecordsNothingAndDoesNotPanic(t *testing.T) {
	var none *Service
	none.IncHTTPRequests("GET", "/", "200")
	none.ObserveHTTPLatency("GET", "/", "200", time.Second)
	none.IncHTTPInFlight("GET", "/")
	none.DecHTTPInFlight("GET", "/")
	none.SetQueueJobs("dead", 1)
	none.IncJobParked("create_charge")
	none.SetOrdersByStatus("refund_required", 1)
	none.IncRateLimited("auth", "limit_exceeded")

	recorder := httptest.NewRecorder()
	none.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("a nil service served %d, want 404", recorder.Code)
	}
}

// A clock that jumped backwards must not put a negative into a histogram,
// where it would sit below every boundary and drag the sum down.
func TestNegativeLatencyIsClamped(t *testing.T) {
	service := New("replica-1")
	service.ObserveHTTPLatency("GET", "/api/v1/orders", "200", -5*time.Second)
	if !strings.Contains(scrape(t, service),
		`app_http_request_duration_seconds_sum{method="GET",path="/api/v1/orders",replica_id="replica-1",status="200"} 0`) {
		t.Error("a negative duration was not clamped to zero")
	}
}

// An empty label value renders as an absent one, which on a dashboard is
// indistinguishable from a metric nobody is recording.
func TestAnEmptyLabelBecomesAPlaceholder(t *testing.T) {
	service := New("")
	service.IncJobParked("")
	body := scrape(t, service)
	if !strings.Contains(body, `app_queue_jobs_parked_total{replica_id="_",type="_"} 1`) {
		t.Error("empty labels were not replaced with a placeholder")
	}
}
