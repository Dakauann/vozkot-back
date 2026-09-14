package notifications

import (
	"fmt"
	"strings"

	domain "vozkot/domain/notification"
)

// Rendering for the channels that reach a phone.
//
// A separate Renderer from the email one rather than a branch inside it, because
// the two have nothing in common: the email renderer parses an HTML tree against
// a layout and a component library at boot, and this one returns a six-character
// string. Putting both behind one type would mean a phone send carrying the
// machinery — and the failure modes — of a template engine it never uses.
//
// Channels is what makes them one Renderer to everything above: the use cases see
// the single domain.Renderer port they always did, and which implementation
// serves a channel is a container decision.

// PhoneCodeRenderer produces the body a code message carries.
//
// Which is the code itself, and that is the whole of it. On these channels the
// WORDING is not ours: an approved WhatsApp authentication template is fixed
// preset text at Meta ("<CODE> is your verification code", plus the security and
// expiry lines the template was approved with), and an SMS body is composed by
// whoever carries it. Rendering a sentence here would either be ignored or be a
// second, divergent copy of wording we do not control.
//
// So the contract is narrow on purpose: this renderer handles exactly the
// templates whose body IS a parameter, and refuses everything else rather than
// inventing a plausible string for it. A receipt on WhatsApp is a real feature
// one day; it is a template registered at Meta and an entry in this map, not a
// fallback that silently sends an HTML receipt as a text message.
type PhoneCodeRenderer struct{}

var _ domain.Renderer = PhoneCodeRenderer{}

// codeDataKey is the key a caller puts the code under, and it is the same key the
// email sign-in template reads. One spelling for both channels, so a use case
// raising a code notification does not need to know which one will carry it.
const codeDataKey = "Code"

// Render returns the code for the one template that has one.
func (PhoneCodeRenderer) Render(channel domain.Channel, name domain.Template, data map[string]any) (string, error) {
	if !channel.Phone() {
		return "", fmt.Errorf("%w: %s", domain.ErrUnknownChannel, channel)
	}
	if name != domain.TemplateSignInCode {
		// Not "unsupported yet" — undeliverable. The Service turns this into a
		// parked job rather than twenty attempts, which is right: no retry adds a
		// template to this renderer.
		return "", fmt.Errorf("%w: %s is not rendered for %s", domain.ErrUnknownTemplate, name, channel)
	}

	code, _ := data[codeDataKey].(string)
	code = strings.TrimSpace(code)
	if code == "" {
		// The data is wrong for the template, which is exactly the case the
		// Service treats as permanent. Refusing here means the provider is never
		// asked to deliver an empty code — which, on a WhatsApp authentication
		// template, Meta answers with a parameter-count error that names neither
		// the template nor the missing value.
		return "", fmt.Errorf("%w: %s carries no %s", domain.ErrUnknownTemplate, name, codeDataKey)
	}
	return code, nil
}

// Channels dispatches a render to whichever Renderer serves that channel.
//
// The Service takes ONE Renderer, and that is the right shape — it renders for
// the channel on the request and does not care how many implementations exist
// behind it. This is the adapter that keeps it true while there is more than one.
type Channels map[domain.Channel]domain.Renderer

var _ domain.Renderer = Channels{}

// NewChannels indexes renderers by the channels they serve.
//
// Every channel is listed explicitly rather than inferred, so a channel added to
// the domain without a renderer fails loudly at its first send instead of
// quietly rendering as email.
func NewChannels(renderers map[domain.Channel]domain.Renderer) Channels {
	indexed := make(Channels, len(renderers))
	for channel, renderer := range renderers {
		if renderer == nil {
			continue
		}
		indexed[channel] = renderer
	}
	return indexed
}

func (c Channels) Render(channel domain.Channel, name domain.Template, data map[string]any) (string, error) {
	renderer, ok := c[channel]
	if !ok {
		return "", fmt.Errorf("%w: %s", domain.ErrUnknownChannel, channel)
	}
	return renderer.Render(channel, name, data)
}
