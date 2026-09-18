package http

import (
	"strings"
	"testing"
)

// Every authenticated route has to sit under a mounted prefix.
//
// THE BUG THIS EXISTS FOR, which shipped: the protected mux is exposed through
// ProtectedPrefixes, and a handler registering `/api/v1/organiser/report` on it
// was never reachable because that prefix was not on the list. The route
// answered 404 rather than 401 and read exactly like a missing handler, while
// every unit test passed, because they call the handler's own mux directly and
// never cross the root router. This asserts the seam those tests skip.
func TestEveryProtectedRouteHasAMount(t *testing.T) {
	// One path per handler that registers on the protected mux. A route added
	// under a new prefix is added here, and fails until it is mounted.
	routes := []string{
		"/api/v1/tickets",
		"/api/v1/orders/ord_1/refund-request",
		"/api/v1/events/evt_1/report",
		"/api/v1/events/evt_1/attendees.csv",
		"/api/v1/refund-requests/req_1/approve",
		"/api/v1/venues/ven_1",
		"/api/v1/layouts/lay_1",
		// The organiser's own account: the three that were unreachable.
		"/api/v1/organiser/balance",
		"/api/v1/organiser/ledger",
		"/api/v1/organiser/report",
	}
	for _, route := range routes {
		if !mounted(route) {
			t.Errorf("%s is registered on the protected mux but no prefix in "+
				"ProtectedPrefixes covers it: it will answer 404, not 401", route)
		}
	}
}

// And the list must not be so broad that it mounts the whole API, which would
// make the test above pass for anything.
func TestTheMountListDoesNotSwallowEverything(t *testing.T) {
	for _, public := range []string{
		"/api/v1/checkout",
		"/auth/login",
		"/healthz",
		"/swagger/index.html",
	} {
		if mounted(public) {
			t.Errorf("%s is covered by ProtectedPrefixes; it is not a protected route", public)
		}
	}
	// A prefix with no trailing-slash twin would mount the collection and not
	// its members, which is the other half of this mistake.
	for _, prefix := range ProtectedPrefixes {
		if strings.HasSuffix(prefix, "/") {
			continue
		}
		if !contains(ProtectedPrefixes, prefix+"/") {
			t.Errorf("%q is mounted but %q is not: paths beneath it would 404", prefix, prefix+"/")
		}
	}
}

func mounted(path string) bool {
	for _, prefix := range ProtectedPrefixes {
		if strings.HasSuffix(prefix, "/") {
			if strings.HasPrefix(path, prefix) {
				return true
			}
			continue
		}
		if path == prefix {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
