package container

import (
	"context"
	"log"
	"net/http"
	"time"
)

// metricsServer is the scrape endpoint, on a listener of its own.
//
// SEPARATE FROM THE API ON PURPOSE, and the reason is exposure rather than
// tidiness. The API listener is the one the internet reaches; /metrics on it
// would be one ingress rule away from publishing this system's internals, and
// it would inherit the CORS headers and the session middleware that belong to
// a public API and not to a scrape. A second listener bound to loopback is
// reachable by a local Prometheus, by a sidecar, or through an SSH tunnel, and
// by nothing else, with no rule to get wrong.
//
// It also carries its own /healthz so the monitoring stack can check the
// process without going through the public router.
type metricsServer struct {
	address string
	server  *http.Server
}

// newMetricsServer returns nil when no address is configured, which disables
// the listener entirely; every method below is nil-safe so the caller needs no
// branch of its own.
func newMetricsServer(address string, handler http.Handler) *metricsServer {
	if address == "" || handler == nil {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", handler)
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok"))
	})
	return &metricsServer{
		address: address,
		server: &http.Server{
			Addr:    address,
			Handler: mux,
			// A scrape is a local, well-behaved client; this only bounds a
			// connection that opens and then says nothing.
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

// Start serves in the background. A failure here is logged and never fatal:
// losing the metrics listener must not take down a box office that is selling
// tickets perfectly well without it.
func (m *metricsServer) Start() {
	if m == nil {
		return
	}
	go func() {
		log.Printf("metrics: serving /metrics on %s", m.address)
		if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics: listener stopped: %v", err)
		}
	}()
}

func (m *metricsServer) Shutdown(ctx context.Context) {
	if m == nil {
		return
	}
	if err := m.server.Shutdown(ctx); err != nil {
		log.Printf("metrics: shutdown: %v", err)
	}
}
