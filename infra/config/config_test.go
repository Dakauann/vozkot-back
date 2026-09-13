package config

import (
	"strings"
	"testing"
)

func TestLoadUsesLocalPostgresDefaultsInDevelopment(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	for _, key := range []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_SSLMODE"} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Database.Host != "localhost" || cfg.Database.Port != "5433" || cfg.Database.Name != "vozkot" {
		t.Fatalf("unexpected development database defaults: %+v", cfg.Database)
	}
}

func TestLoadRequiresDatabaseCredentialsInProduction(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("JWT_SECRET", "production-test-secret")
	for _, key := range []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME"} {
		t.Setenv(key, "")
	}

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DB_HOST is required") {
		t.Fatalf("Load() error = %v, want missing DB_HOST", err)
	}
}
