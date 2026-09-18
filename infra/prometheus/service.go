// Package prometheus is the metrics adapter: the only package in this codebase
// that imports a monitoring library.
//
// Everything above it depends on the small interfaces in domain/metrics, so
// swapping Prometheus for anything else is one package, and a use case that
// reports a parked job does not pull a metrics library into its imports.
package prometheus

import (
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	metricsdomain "vozkot/domain/metrics"
)

// namespace is the fixed, brand-neutral prefix on every metric this system
// publishes, so a metric is app_http_requests_total everywhere.
//
// Deliberately NOT the product name. Metric names have to stay stable across
// deployments or every dashboard and every alert rule becomes per-deployment
// copy. If one Prometheus ever scrapes two brands, they are told apart by a
// label, never by a different metric name.
const namespace = "app"

// latencyBuckets spans a fast cached read to a slow provider round trip.
//
// The interesting region for this system is 50ms to 1s: below that everything
// is a cache hit, and above 2.5s a buyer has already decided something is
// broken. The tail out to 10s exists so a stuck provider call still lands in a
// bucket rather than in +Inf, where it would be invisible to a p95.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Service holds every collector and the registry they live in.
type Service struct {
	httpRequests *prometheus.CounterVec
	httpLatency  *prometheus.HistogramVec
	httpInFlight *prometheus.GaugeVec

	rateLimited *prometheus.CounterVec

	queueJobs  *prometheus.GaugeVec
	jobsParked *prometheus.CounterVec

	ordersByStatus *prometheus.GaugeVec

	registry *prometheus.Registry
}

// New builds the metrics service for one replica.
//
// A PRIVATE registry, never prometheus.DefaultRegisterer: the default is global
// state that any dependency can register into, so a library's own collectors
// would silently appear on our scrape and a name collision would panic at
// init in somebody else's package.
//
// Everything registers through a registerer wrapped with a constant replica_id,
// INCLUDING the Go and process collectors. Without that wrap, go_* and
// process_* would carry only the scrape-time instance label (host:port), and a
// dashboard filtered by replica would show host-scoped memory next to
// replica-scoped request rates, which is how you end up debugging the wrong
// process during an incident.
func New(replicaID string) *Service {
	registry := prometheus.NewRegistry()
	registerer := prometheus.WrapRegistererWith(prometheus.Labels{"replica_id": safeLabel(replicaID)}, registry)

	service := &Service{
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "http", Name: "requests_total",
			Help: "HTTP requests served, by method, normalised route and status code.",
		}, []string{"method", "path", "status"}),

		httpLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "http", Name: "request_duration_seconds",
			Help: "HTTP request latency in seconds, by method, normalised route and status code.", Buckets: latencyBuckets,
		}, []string{"method", "path", "status"}),

		httpInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "http", Name: "in_flight_requests",
			Help: "HTTP requests currently being served.",
		}, []string{"method", "path"}),

		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "rate_limited_total",
			Help: "Requests a limiter refused, or failed to judge. reason=limiter_error means the limiter is broken and traffic is passing unthrottled.",
		}, []string{"limiter", "reason"}),

		queueJobs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "queue", Name: "jobs",
			Help: "Background jobs held in each status. dead is work that needs a person.",
		}, []string{"status"}),

		jobsParked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "queue", Name: "jobs_parked_total",
			Help: "Jobs that exhausted their attempts and were parked, by type.",
		}, []string{"type"}),

		ordersByStatus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "orders", Name: "by_status",
			Help: "Orders resting in each status. refund_required above zero is money owed back to a buyer.",
		}, []string{"status"}),

		registry: registry,
	}

	registerer.MustRegister(
		service.httpRequests,
		service.httpLatency,
		service.httpInFlight,
		service.rateLimited,
		service.queueJobs,
		service.jobsParked,
		service.ordersByStatus,
	)
	registerer.MustRegister(collectors.NewGoCollector())
	registerer.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	service.seed()
	return service
}

// seed publishes a zero for every series whose labels are known in advance.
//
// Without it a healthy system renders "No data" rather than 0, which reads as a
// broken dashboard, and rate() over a counter that has never been incremented
// returns nothing at all, so an alert on a limiter that has never yet failed
// cannot distinguish "fine" from "not wired up".
func (s *Service) seed() {
	for _, status := range []string{"pending", "processing", "done", "dead"} {
		s.queueJobs.WithLabelValues(status).Set(0)
	}
	// Only the status worth alerting on. The rest arrive as they occur; this
	// one has to read zero on a quiet night rather than absent.
	s.ordersByStatus.WithLabelValues("refund_required").Set(0)
	for _, limiter := range []string{"auth", "login", "checkout"} {
		for _, reason := range []string{metricsdomain.RateLimitReasonExceeded, metricsdomain.RateLimitReasonError} {
			s.rateLimited.WithLabelValues(limiter, reason).Add(0)
		}
	}
}

// Handler serves the exposition format for this registry alone.
func (s *Service) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})
}

// ReplicaID resolves who this process is, for the constant label.
//
// The hostname is the right default: in a container it is the container id, on
// a VM it is the host, and both are what an operator would name when asking
// "which one is misbehaving". Vozko requires the variable outright; here a
// missing one must never stop a single-node box office from booting.
func ReplicaID() string {
	if id := strings.TrimSpace(os.Getenv("REPLICA_ID")); id != "" {
		return id
	}
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		return host
	}
	return "unknown"
}

// safeLabel keeps an empty value from creating a silent empty-label series,
// which is indistinguishable on a dashboard from a metric nobody is recording.
func safeLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "_"
	}
	return value
}

// The adapter satisfies every port, checked at compile time rather than at the
// call site that would otherwise discover it at wiring.
var (
	_ metricsdomain.HTTPRecorder      = (*Service)(nil)
	_ metricsdomain.QueueRecorder     = (*Service)(nil)
	_ metricsdomain.OrderRecorder     = (*Service)(nil)
	_ metricsdomain.RateLimitRecorder = (*Service)(nil)
)
