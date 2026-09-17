package qrcode

import (
	"bytes"
	"image/png"
	"testing"

	domain "vozkot/domain/admission"
)

func TestPNGRendersAScannableImage(t *testing.T) {
	code, err := domain.NewCode()
	if err != nil {
		t.Fatalf("NewCode(): %v", err)
	}
	raw, err := NewRenderer().PNG(code)
	if err != nil {
		t.Fatalf("PNG(): %v", err)
	}
	image, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the output is not a PNG: %v", err)
	}
	bounds := image.Bounds()
	if bounds.Dx() < 200 || bounds.Dy() < 200 {
		t.Fatalf("the symbol is %dx%d, too small for a camera to resolve", bounds.Dx(), bounds.Dy())
	}
	if _, err := NewRenderer().PNG(""); err == nil {
		t.Fatal("an empty code was rendered")
	}
}
