package processor

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math/rand"
	"testing"

	"github.com/h2non/bimg"
)

// noisePNG builds a source with per-pixel random color noise so a lossy
// encoder actually has something to compress. A solid fill compresses to
// (near) the same size at any quality setting and would hide a broken
// quality/speed option.
func noisePNG(t *testing.T, width, height int, seed int64) []byte {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.NRGBA{
				R: uint8(r.Intn(256)),
				G: uint8(r.Intn(256)),
				B: uint8(r.Intn(256)),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode noise png: %v", err)
	}
	return buf.Bytes()
}

func solidColorPNG(t *testing.T, width, height int, fill color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode solid png: %v", err)
	}
	return buf.Bytes()
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// TestResizeEncodesEachOutputFormat drives a resize into every supported
// output format and checks the returned bytes really are that container (not
// just that bimg didn't error), plus that the decoded dimensions match what
// was requested.
func TestResizeEncodesEachOutputFormat(t *testing.T) {
	src := noisePNG(t, 240, 160, 7)
	targetWidth, targetHeight := 120, 80

	tests := []struct {
		name   string
		format Format
		check  func(t *testing.T, out []byte)
	}{
		{
			name:   "jpeg",
			format: FormatJPEG,
			check: func(t *testing.T, out []byte) {
				if len(out) < 3 || out[0] != 0xFF || out[1] != 0xD8 || out[2] != 0xFF {
					t.Fatalf("missing JPEG magic bytes: % x", out[:min(len(out), 8)])
				}
			},
		},
		{
			name:   "png",
			format: FormatPNG,
			check: func(t *testing.T, out []byte) {
				if len(out) < 8 || !bytes.Equal(out[:8], pngSignature) {
					t.Fatalf("missing PNG signature: % x", out[:min(len(out), 8)])
				}
			},
		},
		{
			name:   "webp",
			format: FormatWEBP,
			check: func(t *testing.T, out []byte) {
				if len(out) < 12 || string(out[0:4]) != "RIFF" || string(out[8:12]) != "WEBP" {
					t.Fatalf("missing WEBP RIFF/WEBP container signature: % x", out[:min(len(out), 16)])
				}
			},
		},
		{
			name:   "avif",
			format: FormatAVIF,
			check: func(t *testing.T, out []byte) {
				if len(out) < 12 || string(out[4:8]) != "ftyp" || string(out[8:12]) != "avif" {
					t.Fatalf("missing AVIF ftyp/avif signature: % x", out[:min(len(out), 16)])
				}
			},
		},
	}

	p := New()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := p.Resize(src, Options{
				Width:          targetWidth,
				Format:         tc.format,
				JPEGQuality:    80,
				WebPQuality:    75,
				AVIFQuality:    60,
				AVIFSpeed:      8,
				PNGCompression: 6,
			})
			if err != nil {
				t.Fatalf("Resize returned error: %v", err)
			}
			tc.check(t, out)

			size, err := bimg.NewImage(out).Size()
			if err != nil {
				t.Fatalf("inspect result size: %v", err)
			}
			if size.Width != targetWidth || size.Height != targetHeight {
				t.Fatalf("got %dx%d, want %dx%d", size.Width, size.Height, targetWidth, targetHeight)
			}
		})
	}
}

// TestResizeAVIFQualityAffectsSize locks in that avif_quality is actually
// forwarded to the encoder: a low quality must produce a measurably smaller
// payload than a high one for the same noisy source.
func TestResizeAVIFQualityAffectsSize(t *testing.T) {
	src := noisePNG(t, 240, 160, 11)
	p := New()

	low, err := p.Resize(src, Options{Width: 120, Format: FormatAVIF, AVIFQuality: 15, AVIFSpeed: 8})
	if err != nil {
		t.Fatalf("low quality Resize: %v", err)
	}
	high, err := p.Resize(src, Options{Width: 120, Format: FormatAVIF, AVIFQuality: 90, AVIFSpeed: 8})
	if err != nil {
		t.Fatalf("high quality Resize: %v", err)
	}
	if len(low) >= len(high) {
		t.Fatalf("expected low quality AVIF (%d bytes) to be smaller than high quality (%d bytes)", len(low), len(high))
	}
	// Guard against a no-op quality knob producing a coincidentally smaller
	// image for some unrelated reason: require a real gap, not noise.
	if float64(len(low)) > float64(len(high))*0.8 {
		t.Fatalf("quality gap too small to prove avif_quality is honoured: low=%d high=%d", len(low), len(high))
	}
}

// TestResizeJPEGQualityAffectsSize mirrors TestResizeAVIFQualityAffectsSize
// for jpg_quality.
func TestResizeJPEGQualityAffectsSize(t *testing.T) {
	src := noisePNG(t, 240, 160, 12)
	p := New()

	low, err := p.Resize(src, Options{Width: 120, Format: FormatJPEG, JPEGQuality: 15, EnsureOpaque: true})
	if err != nil {
		t.Fatalf("low quality Resize: %v", err)
	}
	high, err := p.Resize(src, Options{Width: 120, Format: FormatJPEG, JPEGQuality: 95, EnsureOpaque: true})
	if err != nil {
		t.Fatalf("high quality Resize: %v", err)
	}
	if len(low) >= len(high) {
		t.Fatalf("expected low quality JPEG (%d bytes) to be smaller than high quality (%d bytes)", len(low), len(high))
	}
	if float64(len(low)) > float64(len(high))*0.8 {
		t.Fatalf("quality gap too small to prove jpg_quality is honoured: low=%d high=%d", len(low), len(high))
	}
}

// TestResizePlainSingleDimensionDownscale exercises the bottom fallback path
// in Resize (a single explicit dimension that does not need to upscale),
// which never goes through the letterbox canvas at all. Width==0 combined
// with Height==0 for the two "both dimensions" cases means only this branch
// applies.
func TestResizePlainSingleDimensionDownscale(t *testing.T) {
	fill := color.NRGBA{R: 30, G: 150, B: 90, A: 255}
	src := solidColorPNG(t, 300, 200, fill)

	p := New()
	result, err := p.Resize(src, Options{
		Width:          150,
		Format:         FormatPNG,
		PNGCompression: 6,
	})
	if err != nil {
		t.Fatalf("Resize returned error: %v", err)
	}

	size, err := bimg.NewImage(result).Size()
	if err != nil {
		t.Fatalf("inspect result size: %v", err)
	}
	if size.Width != 150 || size.Height != 100 {
		t.Fatalf("got %dx%d, want 150x100", size.Width, size.Height)
	}

	decoded, err := png.Decode(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("decode result png: %v", err)
	}
	for _, pt := range []image.Point{{0, 0}, {149, 99}, {75, 50}} {
		got := color.NRGBAModel.Convert(decoded.At(pt.X, pt.Y)).(color.NRGBA)
		if got.A != 255 {
			t.Fatalf("expected fully opaque pixel at %v (no canvas padding), got alpha=%d", pt, got.A)
		}
		if diff(got.R, fill.R) > 8 || diff(got.G, fill.G) > 8 || diff(got.B, fill.B) > 8 {
			t.Fatalf("expected source fill at %v, got %+v", pt, got)
		}
	}
}
