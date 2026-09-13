package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// What is under test here is the SEQUENCING in run(), and it is worth testing
// precisely because it looks like it works either way.
//
// http.Server.Serve returns ErrServerClosed the moment Shutdown closes the
// listener — not when the drain is finished. A main that treated that return as
// "we are done" exited with connections still being served and workers still
// holding jobs, which is a crash wearing a deploy's clothes. Nothing about the
// happy path looks different; the damage only shows up as cut requests and jobs
// stuck in `processing` for the stale window.

// recordingApp is a server-shaped double that reports the order of events.
type recordingApp struct {
	closeListener chan struct{}
	shutdownTakes time.Duration
	shutdownDone  atomic.Bool
	shutdownCtx   atomic.Pointer[time.Time]
	startErr      error
}

func newRecordingApp(shutdownTakes time.Duration) *recordingApp {
	return &recordingApp{closeListener: make(chan struct{}), shutdownTakes: shutdownTakes}
}

func (a *recordingApp) Start() error {
	if a.startErr != nil {
		return a.startErr
	}
	// Blocks like Serve does, and returns the same sentinel the moment the
	// listener is closed.
	<-a.closeListener
	return http.ErrServerClosed
}

func (a *recordingApp) Shutdown(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		a.shutdownCtx.Store(&deadline)
	}
	// Closing the listener is the FIRST thing a real Shutdown does, which is
	// what releases Start while the drain is still ahead of it.
	close(a.closeListener)
	select {
	case <-time.After(a.shutdownTakes):
	case <-ctx.Done():
		return ctx.Err()
	}
	a.shutdownDone.Store(true)
	return nil
}

func TestRunWaitsForTheShutdownToFinish(t *testing.T) {
	app := newRecordingApp(150 * time.Millisecond)
	stop := make(chan struct{})
	close(stop)

	if err := run(app, stop, time.Second); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	// The regression. Without the wait, run() returns as soon as Start() does,
	// which is before Shutdown has drained anything.
	if !app.shutdownDone.Load() {
		t.Fatal("run() returned before the shutdown finished: in-flight requests and jobs would die with the process")
	}
}

func TestRunReportsWhatTheShutdownReported(t *testing.T) {
	app := newRecordingApp(0)
	failure := errors.New("a worker did not drain in time")
	shutdown := &failingShutdownApp{recordingApp: app, err: failure}
	stop := make(chan struct{})
	close(stop)

	err := run(shutdown, stop, time.Second)

	// An unclean drain has to reach the exit code. An orchestrator that is told
	// nothing cannot tell a clean deploy from one that cut a buyer's checkout.
	if !errors.Is(err, failure) {
		t.Fatalf("run() error = %v, want %v", err, failure)
	}
}

type failingShutdownApp struct {
	*recordingApp
	err error
}

func (a *failingShutdownApp) Shutdown(ctx context.Context) error {
	_ = a.recordingApp.Shutdown(ctx)
	return a.err
}

func TestRunReturnsImmediatelyWhenTheServerCannotStart(t *testing.T) {
	app := newRecordingApp(time.Hour)
	app.startErr = errors.New("listen tcp :8080: address already in use")
	// Never signalled: there is no shutdown to wait for, and waiting would hang
	// the process instead of reporting a misconfiguration.
	stop := make(chan struct{})

	done := make(chan error, 1)
	go func() { done <- run(app, stop, time.Second) }()

	select {
	case err := <-done:
		if !errors.Is(err, app.startErr) {
			t.Fatalf("run() error = %v, want the start failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() hung on a server that never started")
	}
}

func TestRunBoundsTheDrainWithTheConfiguredTimeout(t *testing.T) {
	// A drain longer than the budget must be cut, not waited on forever: the
	// orchestrator's own grace period is the next thing to run out, and being
	// killed mid-drain is worse than ending one.
	app := newRecordingApp(time.Hour)
	stop := make(chan struct{})
	close(stop)

	started := time.Now()
	err := run(app, stop, 200*time.Millisecond)
	elapsed := time.Since(started)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run() error = %v, want the deadline to be reported", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("run() took %s: the drain budget was not enforced", elapsed)
	}
	if deadline := app.shutdownCtx.Load(); deadline == nil {
		t.Fatal("Shutdown was given a context with no deadline")
	}
}

// TestRunDrainsAnInFlightRequest is the end-to-end version: a real
// http.Server, a real listener, a real request in flight when the signal
// arrives.
//
// It is the behaviour a deploy depends on. A buyer whose checkout was mid-flight
// must get their answer — the alternative is a request cut after its
// transaction committed, and a client that cannot tell whether it holds tickets.
func TestRunDrainsAnInFlightRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	handlerEntered := make(chan struct{})
	server := &http.Server{
		Handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			close(handlerEntered)
			// Long enough that the shutdown is unmistakably concurrent with it.
			time.Sleep(400 * time.Millisecond)
			response.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(response, `{"status":"served"}`)
		}),
	}
	app := &serverApp{server: server, listener: listener}

	stop := make(chan struct{})
	runDone := make(chan error, 1)
	go func() { runDone <- run(app, stop, 5*time.Second) }()

	type result struct {
		status int
		body   string
		err    error
	}
	requestDone := make(chan result, 1)
	go func() {
		response, err := http.Get(fmt.Sprintf("http://%s/", listener.Addr().String()))
		if err != nil {
			requestDone <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		requestDone <- result{status: response.StatusCode, body: string(body), err: err}
	}()

	// Signal only once the handler is genuinely running.
	select {
	case <-handlerEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("the request never reached the handler")
	}
	close(stop)

	select {
	case got := <-requestDone:
		if got.err != nil {
			t.Fatalf("the in-flight request was cut by the shutdown: %v", got.err)
		}
		if got.status != http.StatusOK || got.body != `{"status":"served"}` {
			t.Fatalf("in-flight request got %d %q, want a complete 200", got.status, got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request never completed")
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not return after the drain")
	}

	// And the listener really is closed: a deploy that kept accepting would
	// never finish.
	if _, err := http.Get(fmt.Sprintf("http://%s/", listener.Addr().String())); err == nil {
		t.Fatal("the server still accepts connections after the shutdown returned")
	}
}

// serverApp is the real thing behind the application interface, which is what
// makes the test above an end-to-end one rather than a test of a double.
type serverApp struct {
	server   *http.Server
	listener net.Listener
}

func (a *serverApp) Start() error                       { return a.server.Serve(a.listener) }
func (a *serverApp) Shutdown(ctx context.Context) error { return a.server.Shutdown(ctx) }
