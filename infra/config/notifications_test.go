package config

import (
	"strings"
	"testing"
)

// clearNotificationEnv makes a test independent of whatever the developer
// happens to have exported.
func clearNotificationEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"RESEND_API_KEY", "RESEND_FROM_EMAIL", "RESEND_FROM_NAME", "RESEND_REPLY_TO",
		"RESEND_MAX_REQUESTS_PER_SECOND", "BRAND_NAME", "BRAND_LEGAL_NAME", "BRAND_CNPJ",
		"BRAND_SITE_URL", "BRAND_SUPPORT_EMAIL", "BRAND_LOGO_URL", "BRAND_FROM_EMAIL",
	} {
		t.Setenv(key, "")
	}
}

// A fresh clone must produce working links and a working logo before anybody
// has set a BRAND_ variable, which is the same bargain the media adapter makes
// with its local directory.
//
// This asserted the logo defaulted to the frontend origin, which is what the
// code did and is the opposite of the comment above it: that origin is
// localhost outside production, and a localhost <img> in an email is a broken
// image in every inbox that opens it. The default is now the embedded mark.
func TestNotificationsDefaultToAWorkingLogo(t *testing.T) {
	clearNotificationEnv(t)

	cfg, err := loadNotifications("http://localhost:3000")
	if err != nil {
		t.Fatalf("loadNotifications() error = %v", err)
	}
	if cfg.Enabled() {
		t.Fatal("notifications are enabled without an API key")
	}
	if cfg.Brand.Name != "Vozko Tickets" {
		t.Errorf("brand name = %q", cfg.Brand.Name)
	}
	if cfg.Brand.SiteURL != "http://localhost:3000" {
		t.Errorf("site URL = %q", cfg.Brand.SiteURL)
	}
	// Links still point at the frontend; only the IMAGE has to travel with the
	// message, because a link is followed by the reader and an image is not.
	if cfg.Brand.LogoURL != EmbeddedLogoSrc {
		t.Errorf("logo URL = %q, want the embedded mark %q", cfg.Brand.LogoURL, EmbeddedLogoSrc)
	}
	if cfg.FromName != "Vozko Tickets" {
		t.Errorf("from name = %q", cfg.FromName)
	}
	if cfg.MaxRPS != 4 {
		t.Errorf("max RPS = %d, want the safe default 4", cfg.MaxRPS)
	}
}

// A deployment with somewhere to host the mark says so, and is then handed the
// URL rather than ~27KB on every message.
func TestAHostedLogoOverridesTheEmbeddedOne(t *testing.T) {
	clearNotificationEnv(t)
	t.Setenv("BRAND_LOGO_URL", "https://cdn.example/vozko.png")

	cfg, err := loadNotifications("http://localhost:3000")
	if err != nil {
		t.Fatalf("loadNotifications() error = %v", err)
	}
	if cfg.Brand.LogoURL != "https://cdn.example/vozko.png" {
		t.Errorf("logo URL = %q, want the configured one", cfg.Brand.LogoURL)
	}
}

// One set of BRAND_ values configures both this box office and Vozko's backend,
// so RESEND_FROM_EMAIL is optional when BRAND_FROM_EMAIL is already there.
func TestFromAddressFallsBackToTheBrand(t *testing.T) {
	clearNotificationEnv(t)
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("BRAND_FROM_EMAIL", "no-reply@tickets.example")
	t.Setenv("BRAND_SUPPORT_EMAIL", "suporte@tickets.example")

	cfg, err := loadNotifications("http://localhost:3000")
	if err != nil {
		t.Fatalf("loadNotifications() error = %v", err)
	}
	if cfg.FromEmail != "no-reply@tickets.example" {
		t.Errorf("from = %q", cfg.FromEmail)
	}
	// A buyer who hits reply should reach a human.
	if cfg.ReplyTo != "suporte@tickets.example" {
		t.Errorf("reply-to = %q", cfg.ReplyTo)
	}
}

// A From address the provider rejects fails EVERY send, and it fails them in a
// worker where nobody is looking. One parse at boot turns a silent outage into
// a refused deploy.
func TestAMalformedFromAddressIsRefusedAtBoot(t *testing.T) {
	clearNotificationEnv(t)
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("RESEND_FROM_EMAIL", "no-reply at tickets.example")

	_, err := loadNotifications("http://localhost:3000")
	if err == nil || !strings.Contains(err.Error(), "RESEND_FROM_EMAIL") {
		t.Fatalf("loadNotifications() error = %v, want a rejected From address", err)
	}
}

func TestAnAPIKeyWithoutAFromAddressIsRefused(t *testing.T) {
	clearNotificationEnv(t)
	t.Setenv("RESEND_API_KEY", "re_test")

	_, err := loadNotifications("http://localhost:3000")
	if err == nil || !strings.Contains(err.Error(), "RESEND_FROM_EMAIL") {
		t.Fatalf("loadNotifications() error = %v, want a required From address", err)
	}
}

// Silence in production is the dangerous shape: every buyer pays and hears
// nothing, and nothing fails, because no job is ever written to fail.
func TestProductionRefusesToStartWithoutAProvider(t *testing.T) {
	clearNotificationEnv(t)
	t.Setenv("APP_ENV", "production")

	_, err := loadNotifications("https://tickets.example")
	if err == nil || !strings.Contains(err.Error(), "RESEND_API_KEY is required in production") {
		t.Fatalf("loadNotifications() error = %v, want a required API key", err)
	}
}

func TestProductionAcceptsAFullyConfiguredProvider(t *testing.T) {
	clearNotificationEnv(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("RESEND_API_KEY", "re_live")
	t.Setenv("RESEND_FROM_EMAIL", "no-reply@tickets.example")
	t.Setenv("BRAND_SITE_URL", "https://tickets.example/")
	t.Setenv("BRAND_LOGO_URL", "https://cdn.tickets.example/logo.png")

	cfg, err := loadNotifications("https://tickets.example")
	if err != nil {
		t.Fatalf("loadNotifications() error = %v", err)
	}
	if !cfg.Enabled() {
		t.Fatal("notifications are not enabled with an API key set")
	}
	// The trailing slash goes, or every link in every email carries a double
	// one.
	if cfg.Brand.SiteURL != "https://tickets.example" {
		t.Errorf("site URL = %q", cfg.Brand.SiteURL)
	}
	if cfg.Brand.LogoURL != "https://cdn.tickets.example/logo.png" {
		t.Errorf("logo URL = %q", cfg.Brand.LogoURL)
	}
}
