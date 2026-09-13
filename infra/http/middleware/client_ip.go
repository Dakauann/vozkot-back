package middleware

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP decides which address a request really came from.
//
// X-Forwarded-For is client-controlled text. Believing it unconditionally means
// any caller can choose their own rate-limit bucket by inventing a header, so a
// per-address limit on login becomes a limit on nobody. Ignoring it entirely is
// no better: behind a load balancer every request would carry the balancer's
// address, the whole internet would share one counter, and the first honest
// burst would lock everyone out.
//
// So the header is believed only when the connection itself came from a network
// that is allowed to set it. An attacker on the internet cannot arrange that.
type ClientIP struct {
	trusted []netip.Prefix
}

// NewClientIP compiles the trusted networks. An empty list trusts no proxy and
// always uses the connecting address.
func NewClientIP(cidrs []string) (*ClientIP, error) {
	resolver := &ClientIP{}
	for _, raw := range cidrs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", raw, err)
		}
		resolver.trusted = append(resolver.trusted, prefix.Masked())
	}
	return resolver, nil
}

// From returns the caller's address, following X-Forwarded-For only from a
// trusted hop.
//
// Only the FIRST entry of the header is read, and only after the peer is
// trusted: the remaining entries were appended by whatever came before and are
// exactly as forgeable as the header was to begin with.
func (c *ClientIP) From(request *http.Request) string {
	peer := peerAddress(request)
	if c == nil || !c.trusts(peer) {
		return peer
	}
	if forwarded := request.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, found := strings.Cut(forwarded, ",")
		if !found {
			first = forwarded
		}
		if candidate := strings.TrimSpace(first); candidate != "" {
			return candidate
		}
	}
	if real := strings.TrimSpace(request.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return peer
}

func (c *ClientIP) trusts(address string) bool {
	if len(c.trusted) == 0 {
		return false
	}
	parsed, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	// A v4 address arriving as ::ffff:a.b.c.d must be matched against v4
	// prefixes, which is what a dual-stack listener produces.
	parsed = parsed.Unmap()
	for _, prefix := range c.trusted {
		if prefix.Contains(parsed) {
			return true
		}
	}
	return false
}

func peerAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}
