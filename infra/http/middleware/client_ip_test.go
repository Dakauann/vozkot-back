package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"vozkot/infra/config"
)

// Who a request came from is a security decision the moment anything is keyed
// on it.
//
// X-Forwarded-For is client-supplied text. Believing it unconditionally lets any
// caller pick their own rate-limit bucket by inventing a header, which turns a
// per-address limit on login into a limit on nobody. Ignoring it entirely is no
// better: behind a load balancer every request carries the balancer's address,
// the whole internet shares one counter, and the first honest burst locks
// everyone out.

func request(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	r.RemoteAddr = remoteAddr
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return r
}

func TestForwardedHeadersAreIgnoredFromAnUntrustedPeer(t *testing.T) {
	// The attack: a caller on the internet naming a different address on every
	// request, so no counter ever reaches its limit.
	clients, err := NewClientIP(config.DefaultTrustedProxyCIDRs)
	if err != nil {
		t.Fatalf("NewClientIP: %v", err)
	}

	got := clients.From(request("198.51.100.7:44321", map[string]string{
		"X-Forwarded-For": "203.0.113.1",
		"X-Real-IP":       "203.0.113.2",
	}))

	if got != "198.51.100.7" {
		t.Fatalf("client = %q, want the address it actually connected from; a spoofed header chose its own rate-limit bucket", got)
	}
}

func TestForwardedHeaderIsHonouredFromATrustedProxy(t *testing.T) {
	// The normal production shape: an ingress inside the VPC, forwarding for a
	// real buyer. Without this every buyer would share the balancer's counter.
	clients, err := NewClientIP(config.DefaultTrustedProxyCIDRs)
	if err != nil {
		t.Fatalf("NewClientIP: %v", err)
	}

	got := clients.From(request("10.0.4.19:5000", map[string]string{
		"X-Forwarded-For": "203.0.113.1, 10.0.4.19",
	}))

	if got != "203.0.113.1" {
		t.Fatalf("client = %q, want the buyer's address from a trusted hop", got)
	}
}

func TestOnlyTheFirstForwardedHopIsRead(t *testing.T) {
	// Everything after the first entry was appended by whatever came before,
	// and is exactly as forgeable as the header was to begin with.
	clients, _ := NewClientIP([]string{"10.0.0.0/8"})

	got := clients.From(request("10.1.1.1:80", map[string]string{
		"X-Forwarded-For": "203.0.113.5, 192.0.2.9, 10.1.1.1",
	}))

	if got != "203.0.113.5" {
		t.Fatalf("client = %q, want the first hop", got)
	}
}

func TestAnEmptyTrustListTrustsNoProxy(t *testing.T) {
	// The explicit "trust nothing" configuration, for a deployment with no
	// proxy in front of it.
	clients, err := NewClientIP(nil)
	if err != nil {
		t.Fatalf("NewClientIP: %v", err)
	}

	got := clients.From(request("10.0.4.19:5000", map[string]string{"X-Forwarded-For": "203.0.113.1"}))

	if got != "10.0.4.19" {
		t.Fatalf("client = %q, want the peer address with no proxy trusted", got)
	}
}

func TestIPv4MappedPeersMatchIPv4Prefixes(t *testing.T) {
	// A dual-stack listener reports a v4 client as ::ffff:10.0.0.5. Without
	// unmapping, a real proxy would silently stop being trusted and every
	// buyer behind it would share one counter.
	clients, _ := NewClientIP([]string{"10.0.0.0/8"})

	got := clients.From(request("[::ffff:10.0.0.5]:5000", map[string]string{"X-Forwarded-For": "203.0.113.1"}))

	if got != "203.0.113.1" {
		t.Fatalf("client = %q, want the forwarded address: an IPv4-mapped proxy was not recognised", got)
	}
}

func TestARemoteAddrWithoutAPortIsStillUsable(t *testing.T) {
	clients, _ := NewClientIP(nil)

	if got := clients.From(request("198.51.100.7", nil)); got != "198.51.100.7" {
		t.Fatalf("client = %q", got)
	}
}

func TestAMalformedTrustedNetworkIsRefusedAtStartup(t *testing.T) {
	// Failing at boot beats a silently empty trust list, which would look like
	// it works and quietly share one rate-limit counter across every buyer.
	if _, err := NewClientIP([]string{"not-a-cidr"}); err == nil {
		t.Fatal("NewClientIP accepted a malformed network")
	}
}

func TestTheDefaultTrustListCoversPrivateNetworks(t *testing.T) {
	clients, err := NewClientIP(config.DefaultTrustedProxyCIDRs)
	if err != nil {
		t.Fatalf("NewClientIP: %v", err)
	}
	for _, peer := range []string{"10.0.0.1:1", "172.16.0.1:1", "192.168.1.1:1", "127.0.0.1:1"} {
		if got := clients.From(request(peer, map[string]string{"X-Forwarded-For": "203.0.113.1"})); got != "203.0.113.1" {
			t.Fatalf("peer %s: client = %q, want the forwarded address", peer, got)
		}
	}
	for _, peer := range []string{"8.8.8.8:1", "203.0.113.9:1"} {
		if got := clients.From(request(peer, map[string]string{"X-Forwarded-For": "203.0.113.1"})); got == "203.0.113.1" {
			t.Fatalf("peer %s was trusted to forward; it is on the public internet", peer)
		}
	}
}
