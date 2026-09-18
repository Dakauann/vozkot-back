package notifications

import (
	_ "embed"

	domain "vozkot/domain/notification"
	"vozkot/infra/config"
)

// The wordmark, carried inside the message rather than fetched from the web.
//
// A hosted logo is the obvious thing and it is what this did: the header
// pointed an <img> at the frontend's /brand path. That URL is only reachable
// from the public internet, and the default one is the frontend ORIGIN the API
// was configured with, which in development is http://localhost:3000. A mail
// client resolves that against the reader's own machine, and Gmail resolves it
// through an image proxy that has never heard of it, so the header rendered as
// a broken image in every message anyone actually opened.
//
// Inline is also what the QR codes already do here, for a related reason, so
// this is the file's existing answer rather than a second one. BRAND_LOGO_URL
// still overrides it, and pointing that at a CDN is the right call at volume:
// this adds ~27KB to every message, which is nothing for one receipt and real
// money across millions of them.
//
// assets/brand-logo.png is a COPY of vozkot-front/public/brand/vozko-tickets-logo.png.
// The two projects build separately, so it cannot be shared by reference;
// changing the mark means changing both, and the palette test next door is the
// only other place these two repos are pinned to each other.
//
//go:embed assets/brand-logo.png
var brandLogoPNG []byte

// Chrome carries the wordmark the layout's header references, and nothing when
// the brand is configured with a hosted logo or the channel is not email.
//
// This is domain.ChromeInliner: the body that references the part and the part
// itself are produced by the same object, so neither the use case nor the
// provider adapter has to know the header has a picture in it.
func (r *Renderer) Chrome(channel domain.Channel) []domain.Inline {
	if channel != domain.ChannelEmail || r.brand.LogoURL != config.EmbeddedLogoSrc {
		return nil
	}
	return []domain.Inline{{
		ContentID:   config.EmbeddedLogoCID,
		Filename:    "brand-logo.png",
		ContentType: "image/png",
		Content:     brandLogoPNG,
	}}
}
