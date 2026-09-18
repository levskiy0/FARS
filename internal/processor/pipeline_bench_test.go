package processor

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/h2non/bimg"
)

// syntheticPhoto builds a JPEG that costs roughly what a catalogue image
// costs: photographic noise rather than flat colour, so the encoders cannot
// cheat on it.
func syntheticPhoto(t testing.TB, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	rng := rand.New(rand.NewSource(1))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8((x*7 + y*3 + rng.Intn(24)) % 256),
				G: uint8((y*5 + rng.Intn(24)) % 256),
				B: uint8((x*3 + rng.Intn(24)) % 256),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// syntheticSprite is the other real case: a PNG with genuine transparency,
// which is what the flatten step exists for.
func syntheticSprite(t testing.TB, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.NRGBA{R: 200, G: 30, B: 30, A: 255}}, image.Point{}, draw.Src)
	// A transparent border, so flattening is observable in the output.
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if x < width/8 || y < height/8 {
				img.Set(x, y, color.NRGBA{})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func benchOptions(format Format, w, h int, opaque bool) Options {
	return Options{
		Width: w, Height: h, Format: format,
		JPEGQuality: 80, WebPQuality: 75, AVIFQuality: 70, AVIFSpeed: 8, PNGCompression: 6,
		EnsureOpaque: opaque,
		MaxWidth:     2000, MaxHeight: 2000,
	}
}

// BenchmarkResize covers the shapes the service actually serves: a photo
// letterboxed onto a fixed canvas (both axes given), the same photo with one
// free axis, and a transparent source that has to be flattened.
func BenchmarkResize(b *testing.B) {
	photo := syntheticPhoto(b, 3000, 2000)
	sprite := syntheticSprite(b, 1200, 1200)
	p := New()

	cases := []struct {
		name   string
		source []byte
		opts   Options
	}{
		{"photo_jpeg_canvas_600x600", photo, benchOptions(FormatJPEG, 600, 600, true)},
		{"photo_webp_canvas_600x600", photo, benchOptions(FormatWEBP, 600, 600, true)},
		{"photo_webp_free_axis_600x0", photo, benchOptions(FormatWEBP, 600, 0, true)},
		{"sprite_png_canvas_600x600", sprite, benchOptions(FormatPNG, 600, 600, false)},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.source)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := p.Resize(tc.source, tc.opts); err != nil {
					b.Fatalf("Resize: %v", err)
				}
			}
		})
	}
}

// TestFlattenSkipMatchesTheFlattenedPipeline is the guard on the optimisation
// that stopped routing opaque sources through a full-resolution flatten. The
// fast path resamples from the original instead of from the flattened PNG, so
// the bytes are not identical; what has to hold is that the image is not.
//
// Digests cannot be pinned here: the exact output depends on the libvips,
// libjpeg and libwebp the host provides, so a golden hash would fail on every
// machine but the one that recorded it. A bound on the pixel difference
// against the pipeline this replaced says the same thing and survives a
// library upgrade.
func TestFlattenSkipMatchesTheFlattenedPipeline(t *testing.T) {
	photo, err := os.ReadFile(filepath.Join("..", "..", "tests", "images", "test.jpg"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	sprite := syntheticSprite(t, 400, 400)

	for _, tc := range []struct {
		name    string
		source  []byte
		opts    Options
		maxMean float64 // mean absolute channel difference, out of 255
	}{
		{"opaque photo to jpeg", photo, benchOptions(FormatJPEG, 200, 200, true), 3},
		{"opaque photo to webp", photo, benchOptions(FormatWEBP, 200, 200, true), 3},
		{"opaque photo to png", photo, benchOptions(FormatPNG, 200, 200, true), 3},
		// A source with real transparency still goes through the flatten, so
		// for it the two pipelines must agree exactly.
		{"transparent sprite to jpeg", sprite, benchOptions(FormatJPEG, 200, 200, true), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fast, err := New().Resize(tc.source, tc.opts)
			if err != nil {
				t.Fatalf("Resize: %v", err)
			}
			slow, err := flattenedReference(t, tc.source, tc.opts)
			if err != nil {
				t.Fatalf("reference pipeline: %v", err)
			}
			mean, worst := compareImages(t, fast, slow)
			if mean > tc.maxMean {
				t.Fatalf("mean channel difference %.3f/255 (worst %.0f) exceeds %.0f", mean, worst, tc.maxMean)
			}
			t.Logf("mean %.3f/255, worst %.0f/255", mean, worst)
		})
	}
}

// flattenedReference reproduces the pipeline as it was before opaque sources
// skipped the flatten: composite onto white at full resolution, shrink that,
// then render the canvas.
func flattenedReference(t *testing.T, source []byte, opts Options) ([]byte, error) {
	t.Helper()
	p := New()
	if converted := toSRGB(source); converted != nil {
		source = converted
	}
	size, err := bimg.NewImage(source).Size()
	if err != nil {
		return nil, err
	}
	flattened, err := p.flattenToWhite(source)
	if err != nil {
		return nil, err
	}
	scale := math.Min(float64(opts.Width)/float64(size.Width), float64(opts.Height)/float64(size.Height))
	stageWidth, stageHeight := 0, 0
	if float64(opts.Width)/float64(size.Width) <= float64(opts.Height)/float64(size.Height) {
		stageWidth = int(math.Round(float64(size.Width) * scale))
	} else {
		stageHeight = int(math.Round(float64(size.Height) * scale))
	}
	stage, err := bimg.NewImage(flattened).Process(bimg.Options{
		Type:          bimg.PNG,
		StripMetadata: true,
		Width:         stageWidth,
		Height:        stageHeight,
		Force:         true,
	})
	if err != nil {
		return nil, err
	}
	return p.renderCanvas(stage, opts)
}

// compareImages returns the mean and worst absolute per-channel difference
// between two encoded images of the same size.
func compareImages(t *testing.T, a, b []byte) (mean float64, worst float64) {
	t.Helper()
	left := decodeForCompare(t, a)
	right := decodeForCompare(t, b)
	if left.Bounds() != right.Bounds() {
		t.Fatalf("bounds differ: %v vs %v", left.Bounds(), right.Bounds())
	}
	var sum float64
	var n int
	for y := left.Bounds().Min.Y; y < left.Bounds().Max.Y; y++ {
		for x := left.Bounds().Min.X; x < left.Bounds().Max.X; x++ {
			r1, g1, b1, _ := left.At(x, y).RGBA()
			r2, g2, b2, _ := right.At(x, y).RGBA()
			for _, d := range []float64{
				math.Abs(float64(r1>>8) - float64(r2>>8)),
				math.Abs(float64(g1>>8) - float64(g2>>8)),
				math.Abs(float64(b1>>8) - float64(b2>>8)),
			} {
				sum += d
				worst = math.Max(worst, d)
				n++
			}
		}
	}
	return sum / float64(n), worst
}

func decodeForCompare(t *testing.T, payload []byte) image.Image {
	t.Helper()
	// WebP is not in the stdlib decoders, so everything is normalised through
	// libvips into PNG before being decoded for comparison.
	asPNG, err := bimg.NewImage(payload).Convert(bimg.PNG)
	if err != nil {
		t.Fatalf("convert for comparison: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(asPNG))
	if err != nil {
		t.Fatalf("decode for comparison: %v", err)
	}
	return decoded
}
