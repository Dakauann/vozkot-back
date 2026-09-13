package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestConcurrencyLimitRejectsOverflowWithoutQueuingIt(t *testing.T) {
	const capacity = 2
	release := make(chan struct{})
	started := make(chan struct{}, capacity)
	guard := NewConcurrencyLimit(capacity)
	handler := guard.Require(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		response.WriteHeader(http.StatusNoContent)
	}))

	var wait sync.WaitGroup
	for index := 0; index < capacity; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/checkout", nil))
		}()
	}
	for index := 0; index < capacity; index++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("admitted request did not start")
		}
	}

	response := httptest.NewRecorder()
	startedAt := time.Now()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/checkout", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("overflow status = %d, want 503", response.Code)
	}
	if time.Since(startedAt) > 100*time.Millisecond {
		t.Fatal("overflow request queued behind admitted work instead of failing fast")
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("overflow response has no Retry-After guidance")
	}

	close(release)
	wait.Wait()
}
