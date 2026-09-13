package http

import (
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSAllowsBrowserAuthModeHeader(t *testing.T) {
	handler := withCORS(stdhttp.HandlerFunc(func(stdhttp.ResponseWriter, *stdhttp.Request) {
		t.Fatal("preflight must not reach the route handler")
	}), "http://localhost:3000")

	request := httptest.NewRequest(stdhttp.MethodOptions, "/auth/login", nil)
	request.Header.Set("Origin", "http://localhost:3000")
	request.Header.Set("Access-Control-Request-Method", stdhttp.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "content-type,x-auth-mode")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != stdhttp.StatusNoContent {
		t.Fatalf("expected 204, got %d", response.Code)
	}
	if !strings.Contains(strings.ToLower(response.Header().Get("Access-Control-Allow-Headers")), "x-auth-mode") {
		t.Fatalf("X-Auth-Mode missing from Access-Control-Allow-Headers: %q", response.Header().Get("Access-Control-Allow-Headers"))
	}
	if !strings.Contains(strings.ToLower(response.Header().Get("Access-Control-Expose-Headers")), "retry-after") {
		t.Fatalf("Retry-After missing from Access-Control-Expose-Headers: %q", response.Header().Get("Access-Control-Expose-Headers"))
	}
}
