package notifications

import (
	"regexp"
	"strings"
	"testing"

	domain "vozkot/domain/notification"
	"vozkot/infra/config"
)

// The header's image has to arrive WITH the message by default.
//
// The regression this guards is not subtle and shipped for a while: the default
// logo URL was built from the frontend origin, which is http://localhost:3000
// outside production, so every header rendered as a broken image in every
// inbox. Nothing failed, nothing was logged, and the only symptom was a
// screenshot from someone who opened one.
func TestTheWordmarkTravelsInsideTheMessageByDefault(t *testing.T) {
	brand := testBrand()
	brand.LogoURL = config.EmbeddedLogoSrc
	renderer, err := NewRenderer(brand)
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}

	parts := renderer.Chrome(domain.ChannelEmail)
	if len(parts) != 1 {
		t.Fatalf("chrome parts = %d, want 1", len(parts))
	}
	if parts[0].ContentID != config.EmbeddedLogoCID {
		t.Errorf("content id = %q, want %q", parts[0].ContentID, config.EmbeddedLogoCID)
	}
	if parts[0].ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png", parts[0].ContentType)
	}
	// A PNG, not an empty embed that would silently attach nothing.
	if len(parts[0].Content) < 1024 || string(parts[0].Content[1:4]) != "PNG" {
		t.Fatalf("embedded logo is not a PNG of a plausible size (%d bytes)", len(parts[0].Content))
	}

	body, err := renderer.Render(domain.ChannelEmail, domain.TemplateSignInCode, map[string]any{"Code": "123456"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The body must point at the part that is actually attached, or the reader
	// gets an attachment and a broken image rather than a header.
	if !strings.Contains(body, `src="`+config.EmbeddedLogoSrc+`"`) {
		t.Error("the rendered header does not reference the inline wordmark")
	}
}

// A deployment that puts the mark on a CDN gets the URL and no attachment, so
// the bytes are not added to every message for nothing.
func TestAHostedLogoIsNotAlsoAttached(t *testing.T) {
	renderer, err := NewRenderer(testBrand()) // a https:// LogoURL
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	if parts := renderer.Chrome(domain.ChannelEmail); len(parts) != 0 {
		t.Errorf("chrome parts = %d, want 0 for a hosted logo", len(parts))
	}
	body, err := renderer.Render(domain.ChannelEmail, domain.TemplateSignInCode, map[string]any{"Code": "123456"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "https://tickets.example/brand/logo.png") {
		t.Error("the hosted logo URL did not reach the header")
	}
}

// Nothing is attached to a channel that cannot carry an attachment.
func TestOnlyEmailCarriesTheWordmark(t *testing.T) {
	brand := testBrand()
	brand.LogoURL = config.EmbeddedLogoSrc
	renderer, err := NewRenderer(brand)
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	if parts := renderer.Chrome(domain.ChannelWhatsApp); len(parts) != 0 {
		t.Errorf("chrome parts = %d on a non-email channel, want 0", len(parts))
	}
}

// The palette the emails are allowed to use, as hex, taken from the light theme
// in vozkot-front/src/app/globals.css.
//
// The templates cannot read that file, they are embedded in a Go binary in a
// different project, so this is the join between them. A colour picked by eye
// is the way a transactional email drifts away from the product it belongs to,
// one near-miss grey at a time, and that is exactly what this set replaced.
var emailPalette = map[string]string{
	"#f6f8f9": "--background",
	"#ffffff": "--card",
	"#eaeef0": "--muted",
	"#dce1e5": "--border",
	"#14171a": "--foreground",
	"#58626a": "--muted-foreground",
	"#00c28e": "--primary",
	"#0e1011": "--primary-foreground",
	"#007a5c": "--primary-ink",
	"#009970": "--primary-edge",
	"#036d3c": "--healthy-ink",
	"#8f4f04": "--warning-ink",
	"#ac1529": "--destructive-ink",
	"#0754a6": "--info-ink",
}

func TestEveryColourInTheTemplatesIsOneTheProductHas(t *testing.T) {
	files, err := templateFiles()
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	// The layout and the components are not in templateFiles, and carry most
	// of the colour between them.
	files = append(files, layoutFile, componentFile)
	hex := regexp.MustCompile(`#[0-9a-fA-F]{6}`)
	for _, file := range files {
		content, err := templateFS.ReadFile(path(file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, found := range hex.FindAllString(string(content), -1) {
			if _, ok := emailPalette[strings.ToLower(found)]; !ok {
				t.Errorf("%s uses %s, which is not one of the product's tokens", file, found)
			}
		}
	}
}
