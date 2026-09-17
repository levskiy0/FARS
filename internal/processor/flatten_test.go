package processor

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"testing"
)

// TestResizeFlattensTransparentPNGToWhiteJPEG reaches flattenToWhite, which
// sits at 0% coverage otherwise: a transparent PNG downscaled into an opaque
// JPEG output only goes through flattenToWhite when both dimensions are
// given and the source needs to shrink (scale < 1) — the plain
// upscale/canvas paths never call it.
func TestResizeFlattensTransparentPNGToWhiteJPEG(t *testing.T) {
	const srcSize = 40
	const centerSize = 20
	fill := color.NRGBA{R: 20, G: 60, B: 200, A: 255}

	// Fully transparent everywhere except a solid opaque block in the
	// middle, so the four corners are unambiguously "was transparent".
	src := image.NewNRGBA(image.Rect(0, 0, srcSize, srcSize))
	offset := (srcSize - centerSize) / 2
	draw.Draw(src, image.Rect(offset, offset, offset+centerSize, offset+centerSize), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode source png: %v", err)
	}

	p := New()
	const target = 20
	result, err := p.Resize(buf.Bytes(), Options{
		Width:       target,
		Height:      target,
		Format:      FormatJPEG,
		JPEGQuality: 90,
	})
	if err != nil {
		t.Fatalf("Resize returned error: %v", err)
	}

	decoded, err := jpeg.Decode(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("decode result jpeg: %v", err)
	}
	bounds := decoded.Bounds()
	if bounds.Dx() != target || bounds.Dy() != target {
		t.Fatalf("got %dx%d, want %dx%d", bounds.Dx(), bounds.Dy(), target, target)
	}

	for _, pt := range []image.Point{{0, 0}, {target - 1, 0}, {0, target - 1}, {target - 1, target - 1}} {
		corner := color.NRGBAModel.Convert(decoded.At(pt.X, pt.Y)).(color.NRGBA)
		if corner.R < 245 || corner.G < 245 || corner.B < 245 {
			t.Fatalf("expected transparent corner %v to flatten to white, got %+v", pt, corner)
		}
	}

	center := color.NRGBAModel.Convert(decoded.At(target/2, target/2)).(color.NRGBA)
	if diff(center.R, fill.R) > 15 || diff(center.G, fill.G) > 15 || diff(center.B, fill.B) > 15 {
		t.Fatalf("expected source fill color at center, got %+v", center)
	}
}
