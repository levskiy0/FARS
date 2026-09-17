package processor

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/h2non/bimg"
)

// pngSourceWithHole builds a PNG whose top-left quadrant is fully
// transparent, so decoders that flatten alpha away are easy to catch.
func pngSourceWithHole(t *testing.T, width, height int, fill color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, 0, width/4, height/4), &image.Uniform{C: color.NRGBA{}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png-with-hole source: %v", err)
	}
	return buf.Bytes()
}

func jpegSource(t *testing.T, width, height int, fill color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode jpeg source: %v", err)
	}
	return buf.Bytes()
}

// webpSource and avifSource are generated with bimg itself (from a PNG built
// in-process) so no binary fixture blobs need to be committed to the repo.
func webpSource(t *testing.T, pngPayload []byte) []byte {
	t.Helper()
	out, err := bimg.NewImage(pngPayload).Process(bimg.Options{Type: bimg.WEBP, Quality: 85})
	if err != nil {
		t.Fatalf("generate webp source via bimg: %v", err)
	}
	return out
}

func avifSource(t *testing.T, pngPayload []byte) []byte {
	t.Helper()
	out, err := bimg.NewImage(pngPayload).Process(bimg.Options{Type: bimg.AVIF, Quality: 70, Speed: 8})
	if err != nil {
		t.Fatalf("generate avif source via bimg: %v", err)
	}
	return out
}

// TestResizeDecodesEverySourceFormat pushes a PNG (opaque and with an alpha
// hole), a JPEG, a WEBP and an AVIF source through a real resize and checks
// libvips actually decoded each container rather than erroring out or
// silently passing bytes through: the resulting dimensions must match the
// request, and the alpha-bearing source must still show up as transparent
// after the resize.
//
// tests/images/test.jpg exists in the repo but is referenced by no Go file;
// fixtures here are generated at test time instead (per the source-format
// audit), so it is left untouched.
func TestResizeDecodesEverySourceFormat(t *testing.T) {
	const srcWidth, srcHeight = 40, 32
	fill := color.NRGBA{R: 10, G: 200, B: 30, A: 255}
	basePNG := solidColorPNG(t, srcWidth, srcHeight, fill)
	alphaPNG := pngSourceWithHole(t, srcWidth, srcHeight, fill)

	tests := []struct {
		name      string
		source    []byte
		wantAlpha bool
	}{
		{name: "png-opaque", source: basePNG},
		{name: "png-with-alpha", source: alphaPNG, wantAlpha: true},
		{name: "jpeg", source: jpegSource(t, srcWidth, srcHeight, fill)},
		{name: "webp", source: webpSource(t, basePNG)},
		{name: "avif", source: avifSource(t, basePNG)},
	}

	p := New()
	const targetWidth = 20
	const targetHeight = srcHeight * targetWidth / srcWidth

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := p.Resize(tc.source, Options{
				Width:          targetWidth,
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
			if size.Width != targetWidth || size.Height != targetHeight {
				t.Fatalf("got %dx%d, want %dx%d", size.Width, size.Height, targetWidth, targetHeight)
			}

			decoded, err := png.Decode(bytes.NewReader(result))
			if err != nil {
				t.Fatalf("decode resized png: %v", err)
			}
			corner := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA)
			if tc.wantAlpha {
				if corner.A > 40 {
					t.Fatalf("expected transparent corner to survive decode+resize, got alpha=%d", corner.A)
				}
			} else if corner.A != 255 {
				t.Fatalf("expected opaque source to stay opaque, got alpha=%d", corner.A)
			}
		})
	}
}
