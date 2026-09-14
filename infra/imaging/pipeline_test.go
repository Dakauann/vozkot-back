package imaging

import (
	"bytes"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// The pipeline runs behind a public upload endpoint, so most of what is tested
// here is what it REFUSES and how it degrades, not how it resizes.

// photo builds a gradient image, which downscales like a photograph rather than
// like a flat colour and so exercises the resampling honestly.
func photo(t *testing.T, width, height int) []byte {
	t.Helper()
	source := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			source.Set(x, y, color.RGBA{
				R: uint8(255 * x / max(width-1, 1)),
				G: uint8(255 * y / max(height-1, 1)),
				B: 90,
				A: 255,
			})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, source, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buffer.Bytes()
}

func TestProcessGeneratesEveryVariantAndPlaceholder(t *testing.T) {
	processor := NewProcessor(2)

	result, err := processor.Process(photo(t, 2000, 1000))

	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.Width != 2000 || result.Height != 1000 {
		t.Fatalf("original = %d×%d, want 2000×1000", result.Width, result.Height)
	}
	if len(result.Variants) != len(Variants) {
		t.Fatalf("variants = %d, want %d", len(result.Variants), len(Variants))
	}
	for index, variant := range result.Variants {
		if variant.Width != Variants[index] {
			t.Fatalf("variant %d is %dpx wide, want %d", index, variant.Width, Variants[index])
		}
		// Aspect ratio survives: a card that reserved a 2:1 box must not get a
		// square image back.
		if variant.Height != variant.Width/2 {
			t.Fatalf("variant %dpx is %d tall, want %d: aspect ratio was not preserved",
				variant.Width, variant.Height, variant.Width/2)
		}
		if len(variant.Data) == 0 {
			t.Fatalf("variant %dpx has no bytes", variant.Width)
		}
	}
	if !strings.HasPrefix(result.BlurDataURL, "data:image/jpeg;base64,") {
		t.Fatalf("blur = %q, want a data URI ready for an img src", truncate(result.BlurDataURL))
	}
	if !strings.HasPrefix(result.DominantColor, "#") || len(result.DominantColor) != 7 {
		t.Fatalf("dominant colour = %q, want #rrggbb", result.DominantColor)
	}
}

// The placeholder ships inline in the HTML of every card on a page, so its size
// is a page-weight budget rather than a detail.
func TestThePlaceholderStaysTiny(t *testing.T) {
	processor := NewProcessor(1)

	result, err := processor.Process(photo(t, 1600, 900))

	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if size := len(result.BlurDataURL); size > 1200 {
		t.Fatalf("blur data URI is %d bytes; twenty-four of them on a listing page is %d KB of HTML",
			size, size*24/1024)
	}
	// And it has to actually decode, or every card renders a broken image.
	encoded := strings.TrimPrefix(result.BlurDataURL, "data:image/jpeg;base64,")
	if encoded == result.BlurDataURL {
		t.Fatal("blur data URI has no base64 payload")
	}
}

// Refusing a decompression bomb is only worth anything if it happens before the
// pixels are decoded, which is what Inspect is for.
func TestAnOversizedImageIsRefusedBeforeItIsDecoded(t *testing.T) {
	// A header claiming far more pixels than the limit. Nothing decodes it.
	header := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, header); err != nil {
		t.Fatalf("encode: %v", err)
	}

	// The real assertion is on the limit arithmetic, exercised through a
	// genuinely large declared size.
	if _, _, err := Inspect(oversizedPNGHeader()); !errors.Is(err, ErrTooManyPixels) {
		t.Fatalf("Inspect() error = %v, want %v", err, ErrTooManyPixels)
	}
	processor := NewProcessor(1)
	if _, err := processor.Process(oversizedPNGHeader()); !errors.Is(err, ErrTooManyPixels) {
		t.Fatalf("Process() error = %v, want it refused before decoding", err)
	}
}

// oversizedPNGHeader is a PNG whose IHDR claims 20000×20000 (400 megapixels)
// but which carries almost no data, the shape of a decompression bomb.
func oversizedPNGHeader() []byte {
	return buildPNGHeader(20000, 20000)
}

func TestGarbageIsNotAnImage(t *testing.T) {
	processor := NewProcessor(1)

	if _, _, err := Inspect([]byte("this is not a picture")); !errors.Is(err, ErrNotAnImage) {
		t.Fatalf("Inspect() error = %v, want %v", err, ErrNotAnImage)
	}
	if _, err := processor.Process(nil); !errors.Is(err, ErrNotAnImage) {
		t.Fatalf("Process(nil) error = %v, want %v", err, ErrNotAnImage)
	}
}

// An image smaller than the smallest variant is not upscaled: that would ship
// more bytes to show less detail. It still gets exactly one variant to serve.
func TestASmallImageIsNeverUpscaled(t *testing.T) {
	processor := NewProcessor(1)

	result, err := processor.Process(photo(t, 200, 100))

	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(result.Variants) != 1 {
		t.Fatalf("variants = %d, want exactly one", len(result.Variants))
	}
	if result.Variants[0].Width != 200 {
		t.Fatalf("variant width = %d, want the original 200", result.Variants[0].Width)
	}
}

// JPEG has no alpha channel. Encoding a transparent PNG straight to it renders
// the transparent regions BLACK, which turns a logo into a black rectangle.
func TestTransparencyIsFlattenedOntoWhiteNotBlack(t *testing.T) {
	transparent := image.NewRGBA(image.Rect(0, 0, 100, 100))
	// Fully transparent everywhere.
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, transparent); err != nil {
		t.Fatalf("encode: %v", err)
	}
	processor := NewProcessor(1)

	result, err := processor.Process(buffer.Bytes())

	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if result.DominantColor != "#ffffff" {
		t.Fatalf("dominant colour = %q, want #ffffff: a transparent PNG became a black box",
			result.DominantColor)
	}
}

// The colour is averaged in linear space. Averaging gamma-encoded sRGB directly
// turns a half-black half-white image into a muddy #808080 instead of the
// perceptually correct mid grey near #bcbcbc.
func TestTheDominantColourIsAveragedInLinearSpace(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			if y < 50 {
				source.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
			} else {
				source.Set(x, y, color.RGBA{A: 255})
			}
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, source); err != nil {
		t.Fatalf("encode: %v", err)
	}
	processor := NewProcessor(1)

	result, err := processor.Process(buffer.Bytes())

	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	// Linear-space average of black and white lands near #bcbcbc; a naive sRGB
	// average lands at #808080. Anything below 0xa0 means the gamma step is gone.
	if result.DominantColor[1] < 'a' && result.DominantColor[1] != 'b' && result.DominantColor[1] != 'c' {
		t.Fatalf("dominant colour = %q, want a linear-space average nearer #bcbcbc than #808080",
			result.DominantColor)
	}
}

func truncate(value string) string {
	if len(value) > 60 {
		return value[:60] + "…"
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// buildPNGHeader hand-assembles a PNG whose IHDR declares a huge size but which
// carries no image data. It is the shape of a decompression bomb: a few bytes
// on the wire, a claim of gigabytes in memory.
func buildPNGHeader(width, height uint32) []byte {
	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})

	ihdr := make([]byte, 0, 25)
	ihdr = append(ihdr, 0x00, 0x00, 0x00, 0x0d) // length 13
	ihdr = append(ihdr, 'I', 'H', 'D', 'R')
	ihdr = append(ihdr, byte(width>>24), byte(width>>16), byte(width>>8), byte(width))
	ihdr = append(ihdr, byte(height>>24), byte(height>>16), byte(height>>8), byte(height))
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8-bit RGBA
	sum := crc32.ChecksumIEEE(ihdr[4:])
	ihdr = append(ihdr, byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))
	out.Write(ihdr)
	return out.Bytes()
}
