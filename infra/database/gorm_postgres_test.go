package database

import (
	"net/url"
	"testing"

	"vozkot/infra/config"
)

func TestDSNEscapesCredentialsAndDatabaseName(t *testing.T) {
	raw := dsn(config.DatabaseConfig{
		Host: "localhost", Port: "5433", User: "dev@user", Password: "p@ss word",
		Name: "vozkot dev", SSLMode: "disable",
	})
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	password, _ := parsed.User.Password()
	if parsed.User.Username() != "dev@user" || password != "p@ss word" {
		t.Fatalf("credentials did not round-trip through DSN")
	}
	if parsed.Path != "/vozkot dev" || parsed.Query().Get("sslmode") != "disable" {
		t.Fatalf("unexpected DSN: %s", parsed.Redacted())
	}
}
