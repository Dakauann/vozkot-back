package notifications

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	domain "vozkot/domain/notification"
	"vozkot/infra/config"
)

// TestWritePreviews renders every template to a directory so a person can open
// them in a browser.
//
// Skipped unless EMAIL_PREVIEW names a directory, because it is a tool and not
// an assertion: nothing here can fail a build, and the reason it lives in the
// test file is that it needs the package's unexported renderer and its embedded
// templates. Email is the one surface with no dev server and no hot reload, so
// without this the only way to see a change is to send yourself a message.
//
//	EMAIL_PREVIEW=../../artifacts go test ./infra/notifications -run WritePreviews
func TestWritePreviews(t *testing.T) {
	out := os.Getenv("EMAIL_PREVIEW")
	if out == "" {
		t.Skip("set EMAIL_PREVIEW to a directory to write previews")
	}
	brand := testBrand()
	brand.LogoURL = config.EmbeddedLogoSrc
	renderer, err := NewRenderer(brand)
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	// A browser cannot resolve cid:, which only means something inside a
	// message, so the preview swaps in the very same bytes as a data URI.
	logo := "data:image/png;base64," +
		base64.StdEncoding.EncodeToString(renderer.Chrome(domain.ChannelEmail)[0].Content)

	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("make %s: %v", out, err)
	}
	for name, data := range previewData() {
		body, err := renderer.Render(domain.ChannelEmail, name, data)
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		body = strings.ReplaceAll(body, config.EmbeddedLogoSrc, logo)
		file := filepath.Join(out, "email-"+string(name)+".html")
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		t.Logf("wrote %s", file)
	}
}

// previewData is one plausible message per template, with the longest realistic
// strings rather than short ones: a five-word event name never shows the wrap
// that a real one does.
func previewData() map[domain.Template]map[string]any {
	order := map[string]any{
		"BuyerName": "Maria Silva", "EventName": "Festival Brisa do Atlântico",
		"OrderReference": "VZK-4821", "Total": "R$ 792,00",
		"StartsAt": "domingo, 20 de setembro de 2026 às 14:20",
		"Place":    "Casa Vozkot, Natal - RN", "TicketTitle": "Camarote direito",
		"Quantity": "2", "UnitPrice": "R$ 396,00",
		"OrderURL": "https://tickets.example/pedidos/VZK-4821",
	}
	confirmed := map[string]any{"PaymentMethod": "PIX", "PaidAt": "17/09/2026 15:02"}
	for key, value := range order {
		confirmed[key] = value
	}
	confirmed["Tickets"] = []map[string]any{
		{"Title": "Camarote direito", "Sequence": 1, "Total": 2, "Code": "8F2K-91QD", "HasQR": false, "ContentID": "t1"},
		{"Title": "Camarote direito", "Sequence": 2, "Total": 2, "Code": "5RT9-24LM", "HasQR": false, "ContentID": "t2"},
	}
	pending := map[string]any{
		"PixCode":   strings.Repeat("00020126580014BR.GOV.BCB.PIX", 6),
		"ExpiresAt": "20/09/2026 15:20",
	}
	for key, value := range order {
		pending[key] = value
	}
	return map[domain.Template]map[string]any{
		domain.TemplateSignInCode:     {"Code": "482913", "ValidForMinutes": "10"},
		domain.TemplateOrderPending:   pending,
		domain.TemplateOrderConfirmed: confirmed,
	}
}
