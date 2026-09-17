package config

import (
	"strings"
	"testing"

	"vozkot/domain/pricing"
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

// The service fee is a property of the build, not of the environment.
//
// This is the test that keeps it that way. The reason the rate was moved into
// code is that both ways of misconfiguring it move real money before anyone
// notices — a forgotten variable charges nothing and silently eats the
// commission on every sale, a mistyped one overcharges buyers — so a future
// change that reintroduces an override would undo the whole point. Setting the
// old variable to a wildly different value must therefore change nothing.
func TestServiceFeeComesFromCodeAndNotTheEnvironment(t *testing.T) {
	t.Setenv("APP_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ServiceFee.BasisPoints != pricing.PlatformBasisPoints {
		t.Fatalf("service fee = %d basis points, want the in-code rate %d",
			cfg.ServiceFee.BasisPoints, pricing.PlatformBasisPoints)
	}
	if cfg.ServiceFee.Free() {
		t.Fatal("the service fee loaded as free; a deployment that charges nothing takes the commission out of our own revenue on every sale")
	}

	// The variable this used to read, set to a value nobody would want.
	t.Setenv("PLATFORM_FEE_BASIS_POINTS", "9999")
	overridden, err := Load()
	if err != nil {
		t.Fatalf("Load() with the retired variable set: %v", err)
	}
	if overridden.ServiceFee.BasisPoints != pricing.PlatformBasisPoints {
		t.Fatalf("PLATFORM_FEE_BASIS_POINTS changed the fee to %d; the rate must come from code alone",
			overridden.ServiceFee.BasisPoints)
	}
}

// Ten per cent, stated once here so that changing the constant is a deliberate
// act with a failing test attached rather than a silent repricing of every
// ticket the platform sells.
func TestTheInCodeRateIsTenPerCent(t *testing.T) {
	if pricing.PlatformBasisPoints != 1_000 {
		t.Fatalf("the platform rate is now %d basis points, not the documented ten per cent. "+
			"If that is intended, update this test, README.md and the service-fee clause of the "+
			"Terms of Service in all four locales, and give buyers the 30 days' notice those Terms promise",
			pricing.PlatformBasisPoints)
	}
}
