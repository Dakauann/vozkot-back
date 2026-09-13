// Package imaging turns an uploaded photo into the handful of things a page
// actually renders: a few fixed widths, a tiny blurred stand-in, and a colour.
//
// Everything here is pure Go. libvips is several times faster and every
// benchmark says so, but it is cgo, and cgo costs the static binary and the
// minimal container this service is deployed as. The work is bounded and off
// the request path, so the trade goes the other way.
//
// Three guards matter more than the speed, and all three exist because this
// runs behind a public upload endpoint:
//
//  1. The dimensions are read from the header BEFORE the pixels are decoded. A
//     decompression bomb is a few kilobytes on the wire and gigabytes in
//     memory, and the only cheap moment to refuse one is before decoding.
//  2. Concurrent decodes are bounded. A 12-megapixel JPEG is roughly 48 MB of
//     heap while it is being resized; unbounded, a burst of uploads is an
//     out-of-memory kill rather than a slow queue.
//  3. Every step degrades rather than fails. An image whose placeholder cannot
//     be produced is still a perfectly good image.
package imaging

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // registered for DecodeConfig and Decode
	"image/jpeg"
	_ "image/png" // registered for DecodeConfig and Decode
	"math"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decode only; x/image has no webp encoder
)

// MaxPixels refuses an image larger than any real photograph a box office
// uploads, before a byte of it is decoded.
//
// Thirty megapixels is a 6000×5000 photo — beyond any poster or stage shot, and
// still 120 MB of RGBA if it were decoded. The check is on the DIMENSIONS from
// the header rather than the file size, because the whole point of a
// decompression bomb is that the two do not correspond.
const MaxPixels = 30_000_000

// Variants are the widths generated at upload.
//
// Fixed widths, generated once, rather than resizing on demand: the read path
// then costs nothing at all, and a CDN with free egress serves them forever.
// The three cover a card, a phone hero and a desktop hero at 2× density.
var Variants = []int{400, 800, 1600}

// blurWidth is how wide the inline placeholder is.
//
// Twenty pixels of JPEG is a few hundred bytes as a data URI — small enough to
// sit in the HTML of every card on a page, and detailed enough to read as the
// image rather than as a smear. It needs no JavaScript at all, which is the
// whole reason it beats a hash for a server-rendered page.
const blurWidth = 20

// blurQuality is deliberately low. The result is blurred by being tiny; spending
// bytes on its fidelity would only make the data URI longer.
const blurQuality = 40

// variantQuality is the quality of the real, served images.
const variantQuality = 80

var (
	ErrTooManyPixels = errors.New("imaging: image dimensions exceed the limit")
	ErrNotAnImage    = errors.New("imaging: data is not a decodable image")
)

// Variant is one generated size.
type Variant struct {
	Width  int
	Height int
	Data   []byte
}

// Result is everything one upload produces.
type Result struct {
	// Width and Height are the ORIGINAL's dimensions, which is what a client
	// needs to reserve the right aspect box.
	Width  int
	Height int
	// Variants are ordered smallest first, and never wider than the original:
	// upscaling a small image would ship more bytes to show less detail.
	Variants []Variant
	// BlurDataURL is a complete `data:image/jpeg;base64,…` string, ready to put
	// straight into an img src.
	BlurDataURL string
	// DominantColor is hex including the leading '#'.
	DominantColor string
}

// Processor generates the variants, bounded.
type Processor struct {
	// inFlight caps concurrent decodes. Pure-Go resampling of a large JPEG is
	// hundreds of milliseconds and tens of megabytes; the semaphore is what
	// turns a burst of uploads into a queue instead of an OOM.
	inFlight chan struct{}
}

// NewProcessor bounds concurrency. Zero or less picks a conservative default.
func NewProcessor(concurrency int) *Processor {
	if concurrency <= 0 {
		concurrency = 2
	}
	return &Processor{inFlight: make(chan struct{}, concurrency)}
}

// Inspect reads the dimensions from the header without decoding the pixels.
//
// Separate from Process so a caller can refuse an image before paying for it,
// which is the only order in which that refusal is worth anything.
func Inspect(data []byte) (width, height int, err error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrNotAnImage, err)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return 0, 0, ErrNotAnImage
	}
	if config.Width*config.Height > MaxPixels {
		return config.Width, config.Height, fmt.Errorf(
			"%w: %d×%d is %d pixels, over the %d limit",
			ErrTooManyPixels, config.Width, config.Height, config.Width*config.Height, MaxPixels)
	}
	return config.Width, config.Height, nil
}

// Process decodes once and derives everything from that one decode.
func (p *Processor) Process(data []byte) (*Result, error) {
	if _, _, err := Inspect(data); err != nil {
		return nil, err
	}

	// Bounded from here: everything below holds the decoded image in memory.
	p.inFlight <- struct{}{}
	defer func() { <-p.inFlight }()

	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAnImage, err)
	}
	bounds := source.Bounds()
	result := &Result{Width: bounds.Dx(), Height: bounds.Dy()}

	for _, width := range Variants {
		// Never upscale. A 300px poster rendered at 1600 is more bytes for less
		// detail, and the loader picking the next size up handles it correctly.
		if width > result.Width {
			continue
		}
		resized := scale(source, width)
		encoded, err := encodeJPEG(resized, variantQuality)
		if err != nil {
			return nil, err
		}
		result.Variants = append(result.Variants, Variant{
			Width:  resized.Bounds().Dx(),
			Height: resized.Bounds().Dy(),
			Data:   encoded,
		})
	}
	// An image smaller than the smallest variant still needs one to serve.
	if len(result.Variants) == 0 {
		encoded, err := encodeJPEG(toRGBA(source), variantQuality)
		if err != nil {
			return nil, err
		}
		result.Variants = append(result.Variants, Variant{
			Width: result.Width, Height: result.Height, Data: encoded,
		})
	}

	// The placeholder and the colour both come off the same tiny thumbnail, so
	// the expensive resample happens once.
	thumbnail := scale(source, blurWidth)
	if encoded, err := encodeJPEG(thumbnail, blurQuality); err == nil {
		result.BlurDataURL = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(encoded)
	}
	result.DominantColor = averageColor(thumbnail)
	return result, nil
}

// scale resizes to a target width, preserving the aspect ratio.
//
// CatmullRom is the filter to use here: x/image documents it as a sharp cubic
// that is faster than Lanczos with comparable results, and photographic
// downscaling is exactly what it is for. ApproxBiLinear would be quicker and
// visibly softer, which on a poster is the difference between artwork and mush.
func scale(source image.Image, width int) *image.RGBA {
	bounds := source.Bounds()
	if width >= bounds.Dx() {
		return toRGBA(source)
	}
	height := int(math.Round(float64(bounds.Dy()) * float64(width) / float64(bounds.Dx())))
	if height < 1 {
		height = 1
	}
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(target, target.Bounds(), source, bounds, xdraw.Over, nil)
	return target
}

func toRGBA(source image.Image) *image.RGBA {
	if already, ok := source.(*image.RGBA); ok {
		return already
	}
	bounds := source.Bounds()
	target := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Src)
	return target
}

// encodeJPEG flattens transparency onto white first.
//
// JPEG has no alpha channel, and encoding an RGBA image with transparent
// regions straight to it renders them BLACK — a PNG logo on a transparent
// background becomes a black rectangle, which is the kind of bug that ships
// because nobody uploads a transparent PNG until a customer does.
func encodeJPEG(source *image.RGBA, quality int) ([]byte, error) {
	flattened := image.NewRGBA(source.Bounds())
	draw.Draw(flattened, flattened.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flattened, flattened.Bounds(), source, source.Bounds().Min, draw.Over)

	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, flattened, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("imaging: encode: %w", err)
	}
	return buffer.Bytes(), nil
}

// averageColor is the last-resort placeholder: one colour behind the box while
// even the blur is loading.
//
// Two things it has to get right, and both were bugs first:
//
// It averages in LINEAR space. Averaging gamma-encoded sRGB directly is the
// classic mistake that turns a photo of a bright sky over dark ground into an
// implausible mud-grey; linearising first gives the colour a person would name.
//
// And it composites onto white exactly as encodeJPEG does, rather than skipping
// transparent pixels. Skipping them means a fully transparent PNG averages over
// nothing and comes back BLACK, while the image actually served is white — so
// the placeholder would be the photographic negative of the thing it stands in
// for. The colour has to describe what a viewer will see.
func averageColor(source *image.RGBA) string {
	bounds := source.Bounds()
	var red, green, blue, count float64
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pixel := source.RGBAAt(x, y)
			// Composite over white in linear space. Go's RGBA is
			// alpha-premultiplied, so the source term is already scaled and the
			// background contributes (1 - alpha).
			alpha := float64(pixel.A) / 255
			red += toLinear(pixel.R) + (1 - alpha)
			green += toLinear(pixel.G) + (1 - alpha)
			blue += toLinear(pixel.B) + (1 - alpha)
			count++
		}
	}
	if count == 0 {
		return "#ffffff"
	}
	return fmt.Sprintf("#%02x%02x%02x",
		toSRGB(red/count), toSRGB(green/count), toSRGB(blue/count))
}

func toLinear(value uint8) float64 {
	channel := float64(value) / 255
	if channel <= 0.04045 {
		return channel / 12.92
	}
	return math.Pow((channel+0.055)/1.055, 2.4)
}

func toSRGB(channel float64) uint8 {
	var encoded float64
	if channel <= 0.0031308 {
		encoded = channel * 12.92
	} else {
		encoded = 1.055*math.Pow(channel, 1/2.4) - 0.055
	}
	value := math.Round(encoded * 255)
	if value < 0 {
		value = 0
	}
	if value > 255 {
		value = 255
	}
	return uint8(value)
}
