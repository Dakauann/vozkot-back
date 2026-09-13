package mercadopago

import (
	"net/http"
	"testing"
)

func TestDefaultClientKeepsAConnectionPoolForConcurrentWorkers(t *testing.T) {
	built := NewClient("token", "https://api.example.test").(*client)
	httpClient, ok := built.http.(*http.Client)
	if !ok {
		t.Fatalf("default HTTP client = %T", built.http)
	}
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport = %T", httpClient.Transport)
	}
	if transport.MaxIdleConnsPerHost != defaultMaxConnectionsPerHost ||
		transport.MaxConnsPerHost != defaultMaxConnectionsPerHost {
		t.Fatalf("provider connection limits = idle %d, total %d; want %d",
			transport.MaxIdleConnsPerHost, transport.MaxConnsPerHost, defaultMaxConnectionsPerHost)
	}
}
