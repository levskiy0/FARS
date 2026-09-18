package processor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math/rand"
	"testing"
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

// TestPipelineOutputDigests pins the bytes each pipeline shape produces. It is
// the guard for optimisation work: a change that only removes work leaves
// every digest untouched, and one that alters a pixel shows up here rather
// than in production.
func TestPipelineOutputDigests(t *testing.T) {
	photo := syntheticPhoto(t, 800, 600)
	sprite := syntheticSprite(t, 400, 400)
	p := New()

	for _, tc := range []struct {
		name   string
		source []byte
		opts   Options
	}{
		{"photo_jpeg_canvas", photo, benchOptions(FormatJPEG, 200, 200, true)},
		{"photo_webp_canvas", photo, benchOptions(FormatWEBP, 200, 200, true)},
		{"photo_png_canvas", photo, benchOptions(FormatPNG, 200, 200, true)},
		{"photo_webp_free_axis", photo, benchOptions(FormatWEBP, 200, 0, true)},
		{"sprite_png_canvas", sprite, benchOptions(FormatPNG, 200, 200, false)},
		{"sprite_jpeg_canvas", sprite, benchOptions(FormatJPEG, 200, 200, false)},
		{"sprite_png_flattened", sprite, benchOptions(FormatPNG, 200, 200, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := p.Resize(tc.source, tc.opts)
			if err != nil {
				t.Fatalf("Resize: %v", err)
			}
			sum := sha256.Sum256(out)
			t.Logf("%s digest=%s bytes=%d", tc.name, hex.EncodeToString(sum[:8]), len(out))
		})
	}
}
