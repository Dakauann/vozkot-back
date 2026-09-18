// Package qrcode renders an admission code as a scannable image.
//
// Behind a port in domain/admission so the use cases never import an encoder,
// and so a future change, a different library, a vector output, a provider
// that renders server-side, is one adapter rather than a change everywhere a
// ticket is drawn.
package qrcode

import (
	"fmt"

	encoder "github.com/skip2/go-qrcode"

	domain "vozkot/domain/admission"
)

// Defaults chosen for the two places a ticket is read.
//
// Error correction MEDIUM, not HIGH: a phone screen with a thumbprint on it and
// a sheet of paper folded in a pocket are both well within medium's 15%, and
// high would make the symbol denser for a scanner in a dark doorway to resolve.
// Low is too little for paper.
//
// 256 pixels is the useful size for both destinations. In an email it renders
// at about 180 CSS pixels on a phone, which is comfortably above the ~140 a
// camera needs for a 12-character payload; on the ticket page it is upscaled
// by CSS and stays crisp because the module edges are on pixel boundaries.
const (
	Level = encoder.Medium
	Size  = 256
)

// Renderer encodes admission codes as PNG.
type Renderer struct{}

func NewRenderer() *Renderer { return &Renderer{} }

var _ domain.CodeRenderer = (*Renderer)(nil)

// PNG renders one code.
//
// The payload is the BARE code, the same twelve characters the printed line
// shows, without the grouping hyphens. Not a URL: a URL would make every
// ticket depend on a hostname that has to stay valid for as long as anybody
// holds one, would triple the payload and therefore the density, and would send
// a holder who scans their own ticket to a page they cannot open. A scanner
// reads the characters and the door looks them up.
func (r *Renderer) PNG(code domain.Code) ([]byte, error) {
	if code == "" {
		return nil, fmt.Errorf("qrcode: refusing to render an empty code")
	}
	png, err := encoder.Encode(code.String(), Level, Size)
	if err != nil {
		return nil, fmt.Errorf("qrcode: encode admission code: %w", err)
	}
	return png, nil
}
