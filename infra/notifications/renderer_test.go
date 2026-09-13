package notifications

import (
	"strings"
	"testing"

	domain "vozkot/domain/notification"
	"vozkot/infra/config"
)

func testBrand() config.BrandConfig {
	return config.BrandConfig{
		Name:         "Vozko Tickets",
		LegalName:    "Vozko Tecnologia LTDA",
		CNPJ:         "00.000.000/0001-00",
		SiteURL:      "https://tickets.example",
		SupportEmail: "suporte@tickets.example",
		LogoURL:      "https://tickets.example/brand/logo.png",
	}
}

// Every template file that ships must be reachable, and every template the
// domain names must have a file. A template added to the directory and not
// registered is dead weight; one named in the domain without a file is a
// notification that can only ever be parked.
func TestEveryTemplateIsRegistered(t *testing.T) {
	onDisk, err := templateFiles()
	if err != nil {
		t.Fatalf("list templates: %v", err)
	}
	registered := map[string]bool{}
	for _, file := range files {
		registered[file] = true
	}
	for _, file := range onDisk {
		if !registered[file] {
			t.Errorf("template %s is embedded but not registered in files", file)
		}
	}
	if len(onDisk) != len(files) {
		t.Errorf("registered %d template(s), %d on disk", len(files), len(onDisk))
	}

	for _, name := range []domain.Template{domain.TemplateOrderPending, domain.TemplateOrderConfirmed} {
		if !name.Valid() {
			t.Errorf("%s is not a valid domain template", name)
		}
		if _, ok := files[name]; !ok {
			t.Errorf("domain template %s has no file", name)
		}
	}
}

// Parsing happens once, at construction, so a broken template fails a deploy
// rather than one buyer's receipt.
func TestNewRendererParsesEveryTemplate(t *testing.T) {
	renderer, err := NewRenderer(testBrand())
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	if renderer.Templates() != len(files) {
		t.Fatalf("loaded %d template(s), want %d", renderer.Templates(), len(files))
	}
}

func TestRenderCarriesTheBrandAndTheFacts(t *testing.T) {
	renderer, err := NewRenderer(testBrand())
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}

	body, err := renderer.Render(domain.ChannelEmail, domain.TemplateOrderConfirmed, map[string]any{
		"OrderReference": "A1B2C3D4",
		"BuyerName":      "Maria",
		"Total":          "R$ 480,00",
		"UnitPrice":      "R$ 240,00",
		"Quantity":       2,
		"PaymentMethod":  "PIX",
		"PaidAt":         "13/09/2026 22:04",
		"EventName":      "Festival Aurora",
		"TicketTitle":    "Pista",
		"StartsAt":       "sábado, 3 de outubro de 2026, 22h00",
		"Place":          "Arena · São Paulo",
		"OrderURL":       "https://tickets.example/pedidos/ord_1",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		"Vozko Tickets", "Vozko Tecnologia LTDA", "00.000.000/0001-00",
		"https://tickets.example/brand/logo.png", "suporte@tickets.example",
		"A1B2C3D4", "Maria", "R$ 480,00", "Festival Aurora", "Pista",
		"Arena · São Paulo", "https://tickets.example/pedidos/ord_1",
		"13/09/2026 22:04",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered body is missing %q", want)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(body), "<!DOCTYPE html>") {
		t.Error("rendered body is not a full document")
	}
}

// A missing key must never reach a buyer as Go's "<no value>". The use case
// guarantees every key is set; this is the check that the guarantee is worth
// something, by rendering with the keys absent and requiring the marker not to
// appear — which it will not, because every optional value is behind a guard.
func TestRenderNeverEmitsNoValue(t *testing.T) {
	renderer, err := NewRenderer(testBrand())
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	for name := range files {
		body, err := renderer.Render(domain.ChannelEmail, name, map[string]any{
			// Only what the components consume as arguments. Everything else
			// is deliberately absent.
			"OrderReference":  "A1B2C3D4",
			"Total":           "R$ 10,00",
			"UnitPrice":       "R$ 10,00",
			"Quantity":        1,
			"PaymentMethod":   "PIX",
			"EventName":       "",
			"TicketTitle":     "",
			"StartsAt":        "",
			"Place":           "",
			"PaidAt":          "",
			"PaymentDeadline": "",
		})
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		if strings.Contains(body, "<no value>") {
			t.Errorf("%s rendered a missing key as \"<no value>\"", name)
		}
	}
}

// The PIX payload is buyer-supplied only in the sense that it comes back from a
// provider; it still goes through the escaper like everything else.
func TestRenderEscapesInjectedMarkup(t *testing.T) {
	renderer, err := NewRenderer(testBrand())
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	body, err := renderer.Render(domain.ChannelEmail, domain.TemplateOrderPending, map[string]any{
		"OrderReference":  "A1B2C3D4",
		"Total":           "R$ 10,00",
		"UnitPrice":       "R$ 10,00",
		"Quantity":        1,
		"PaymentMethod":   "PIX",
		"PaymentDeadline": "hoje",
		"HasPix":          true,
		"PixCopyPaste":    `<script>alert(1)</script>`,
		"EventName":       "",
		"TicketTitle":     "",
		"StartsAt":        "",
		"Place":           "",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("markup in the PIX payload was not escaped")
	}
}

func TestRenderRejectsUnknownChannelAndTemplate(t *testing.T) {
	renderer, err := NewRenderer(testBrand())
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	if _, err := renderer.Render(domain.Channel("whatsapp"), domain.TemplateOrderConfirmed, nil); err == nil {
		t.Error("expected an error for a channel with no renderer")
	}
	if _, err := renderer.Render(domain.ChannelEmail, domain.Template("nope"), nil); err == nil {
		t.Error("expected an error for an unknown template")
	}
}

// The caller's map arrives decoded from a job payload and belongs to the
// caller; a renderer that writes into it is a data race waiting for load.
func TestRenderDoesNotMutateCallerData(t *testing.T) {
	renderer, err := NewRenderer(testBrand())
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	data := map[string]any{"OrderReference": "A1", "Total": "R$ 1,00", "UnitPrice": "R$ 1,00", "Quantity": 1}
	if _, err := renderer.Render(domain.ChannelEmail, domain.TemplateOrderConfirmed, data); err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, found := data["Brand"]; found {
		t.Error("Render wrote Brand into the caller's map")
	}
}
