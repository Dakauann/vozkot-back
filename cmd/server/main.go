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

	shutdownContext, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	go func() {
		<-shutdownContext.Done()
		log.Println("shutdown signal received")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := app.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()

	log.Printf("%s listening on :%s", cfg.AppName, cfg.Port)
	if err := app.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("start server: %v", err)
	}
}

func loadEnv() {
	if os.Getenv("APP_ENV") == "production" {
		return
	}
	if err := godotenv.Load(); err != nil {
		log.Println("godotenv: no .env file found, continuing with the environment")
	}
}
