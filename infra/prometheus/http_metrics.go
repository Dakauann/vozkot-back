package prometheus

import "time"

// The HTTP recorders. Every one is nil-safe on both the service and the
// collector, so metrics stay optional at the call site and a handler under test
// can be given nothing at all.

func (s *Service) IncHTTPRequests(method, path, status string) {
	if s == nil || s.httpRequests == nil {
		return
	}
	s.httpRequests.WithLabelValues(safeLabel(method), safeLabel(path), safeLabel(status)).Inc()
}

func (s *Service) ObserveHTTPLatency(method, path, status string, elapsed time.Duration) {
	if s == nil || s.httpLatency == nil {
		return
	}
	// A clock that went backwards must not put a negative into a histogram;
	// it would sit below every bucket boundary and drag the sum down.
	if elapsed < 0 {
		elapsed = 0
	}
	s.httpLatency.WithLabelValues(safeLabel(method), safeLabel(path), safeLabel(status)).Observe(elapsed.Seconds())
}

func (s *Service) IncHTTPInFlight(method, path string) {
	if s == nil || s.httpInFlight == nil {
		return
	}
	s.httpInFlight.WithLabelValues(safeLabel(method), safeLabel(path)).Inc()
}

func (s *Service) DecHTTPInFlight(method, path string) {
	if s == nil || s.httpInFlight == nil {
		return
	}
	s.httpInFlight.WithLabelValues(safeLabel(method), safeLabel(path)).Dec()
}
