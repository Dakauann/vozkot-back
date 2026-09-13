// Package notifications is the outside world a message actually leaves through:
// the HTML it is rendered into, and the provider that carries it.
//
// Nothing above this package names Resend or HTML. The use cases hand down a
// template name, a recipient and a map of data; what comes back out is an email
// because that is what the container wired in, and it becomes a WhatsApp
// message the day a second Sender is registered here.
package notifications

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"strings"

	domain "vozkot/domain/notification"
	"vozkot/infra/config"
)

// templateFS embeds the templates in the binary.
//
// Not read from disk at runtime, and that is deliberate. Vozko resolves a
// templates directory at send time and falls back through several candidate
// paths when it misses; that machinery exists only because the files are not in
// the binary. Embedded, the path question does not arise, a container image
// cannot ship without them, and a receipt cannot fail because a volume was
// mounted somewhere else.
//
//go:embed templates/*.html
var templateFS embed.FS

const (
	templatesDir  = "templates"
	layoutFile    = "_layout.html"
	componentFile = "_components.html"
)

// emailFuncs are available inside every email template. dict is what lets a
// template pass named arguments to a shared component; safeHTML injects
// pre-built markup without escaping and is used by nothing yet, on purpose.
var emailFuncs = template.FuncMap{
	"dict": func(values ...any) (map[string]any, error) {
		if len(values)%2 != 0 {
			return nil, errors.New("dict: needs an even number of arguments")
		}
		pairs := make(map[string]any, len(values)/2)
		for index := 0; index < len(values); index += 2 {
			key, ok := values[index].(string)
			if !ok {
				return nil, errors.New("dict: keys must be strings")
			}
			pairs[key] = values[index+1]
		}
		return pairs, nil
	},
	"safeHTML": func(value string) template.HTML { return template.HTML(value) },
}

// Renderer turns a template plus its data into an email body.
//
// Every template is parsed ONCE, at construction. Two things follow from that,
// and both matter at volume: a template with a syntax error or a missing
// component fails the boot instead of one buyer's receipt, and rendering a
// confirmation costs a buffer and a walk of an already-parsed tree rather than
// three file reads and three parses per message.
type Renderer struct {
	brand     config.BrandConfig
	templates map[domain.Template]*template.Template
}

var _ domain.Renderer = (*Renderer)(nil)

// files maps a template name onto the file that renders it for email. A name
// with no entry cannot be sent, which is why domain.Template.Valid and this map
// are checked against each other by the package's test.
var files = map[domain.Template]string{
	domain.TemplateOrderPending:   "order_pending.html",
	domain.TemplateOrderConfirmed: "order_confirmed.html",
}

// NewRenderer parses every email template against the shared layout and
// components.
func NewRenderer(brand config.BrandConfig) (*Renderer, error) {
	parsed := make(map[domain.Template]*template.Template, len(files))
	for name, file := range files {
		content, err := template.New(layoutFile).Funcs(emailFuncs).ParseFS(templateFS,
			path(layoutFile), path(componentFile), path(file))
		if err != nil {
			return nil, fmt.Errorf("notifications: parse email template %s: %w", file, err)
		}
		parsed[name] = content
	}
	return &Renderer{brand: brand, templates: parsed}, nil
}

// Templates reports how many are loaded, for the boot log.
func (r *Renderer) Templates() int { return len(r.templates) }

// Render produces the body one channel carries for one template.
func (r *Renderer) Render(channel domain.Channel, name domain.Template, data map[string]any) (string, error) {
	if channel != domain.ChannelEmail {
		return "", fmt.Errorf("%w: %s", domain.ErrUnknownChannel, channel)
	}
	parsed, ok := r.templates[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", domain.ErrUnknownTemplate, name)
	}

	// The caller's map is copied rather than written into. It arrived decoded
	// from a job payload and belongs to the caller; a renderer that quietly
	// adds a key to it is the kind of shared mutable state that only shows up
	// as a race under load.
	values := make(map[string]any, len(data)+1)
	for key, value := range data {
		values[key] = value
	}
	values["Brand"] = r.brand

	var rendered bytes.Buffer
	if err := parsed.ExecuteTemplate(&rendered, layoutFile, values); err != nil {
		return "", fmt.Errorf("notifications: render %s: %w", name, err)
	}
	return rendered.String(), nil
}

func path(file string) string { return templatesDir + "/" + file }

// templateFiles lists the embedded content templates, for the test that checks
// none was added without being registered above.
func templateFiles() ([]string, error) {
	entries, err := fs.ReadDir(templateFS, templatesDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "_") {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}
