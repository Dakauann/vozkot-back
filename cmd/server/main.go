package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	_ "vozkot/docs"
	"vozkot/infra/config"
	"vozkot/infra/container"
)

// @title		Vozkot Tickets API
// @version		1.0
// @description	API base para autenticação e gestão de tickets, organizada com Clean Architecture.
// @BasePath		/
// @schemes		http https
// @securityDefinitions.apikey	BearerAuth
// @in			header
// @name		Authorization
// @description	Use o formato: Bearer {token}. Navegadores também podem autenticar por cookies httpOnly.

func main() {
	loadEnv()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load configuration: %v", err)
	}

	app, err := container.New(cfg)
	if err != nil {
		log.Fatalf("build application: %v", err)
	}

	signals, stopListening := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stopListening()

	log.Printf("%s listening on :%s", cfg.AppName, cfg.Port)
	if err := run(app, signals.Done(), cfg.ShutdownTimeout); err != nil {
		log.Fatalf("server: %v", err)
	}
	log.Println("shutdown complete")
}

// application is what main drives: something that serves until it is told to
// stop, and that stops in an orderly way when asked.
//
// An interface rather than *container.Container so the sequencing below — the
// part that was wrong, and the part that is hard to get right — can be tested
// without a database, a broker and a payment provider.
type application interface {
	Start() error
	Shutdown(ctx context.Context) error
}

// run serves until stop fires, then shuts down and WAITS for the shutdown to
// finish before returning.
//
// The waiting is the whole point, and its absence was a bug that made every
// deploy behave like a crash. Start() returns http.ErrServerClosed the instant
// Shutdown closes the listener — not when the drain is done — so a main that
// returned there killed the process with connections still being served, jobs
// still mid-charge, and the careful drain in container.Shutdown never reaching
// its end. Those jobs then sat marked processing until the five-minute stale
// sweep, and their buyers waited that long for a PIX code. Go's own
// documentation for Server.Shutdown warns about exactly this: "Shutdown does
// not attempt to close nor wait for hijacked connections... the caller should
// separately notify of the return and wait for it."
//
// So the goroutine reports the shutdown's OUTCOME rather than only starting it,
// and the last thing run does is read that report.
func run(app application, stop <-chan struct{}, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	shutdown := make(chan error, 1)

	go func() {
		<-stop
		log.Printf("shutdown signal received; draining for up to %s", timeout)
		// A fresh context, deliberately not derived from anything the signal
		// cancelled: the drain has to outlive the thing that asked for it.
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		shutdown <- app.Shutdown(ctx)
	}()

	if err := app.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// The server never got going — a port already in use, a bad address.
		// There is no drain to wait for.
		return err
	}
	// ErrServerClosed means Shutdown closed the listener, so the goroutine
	// above is running and will report. Block until it has.
	return <-shutdown
}

func loadEnv() {
	if os.Getenv("APP_ENV") == "production" {
		return
	}
	if err := godotenv.Load(); err != nil {
		log.Println("godotenv: no .env file found, continuing with the environment")
	}
}
