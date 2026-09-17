package processor

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"

	"github.com/h2non/bimg"

	_ "image/jpeg"
)

func TestResizeCentersImageWithoutUpscaling(t *testing.T) {
	canvasSize := 20
	srcSize := 10
	src := image.NewNRGBA(image.Rect(0, 0, srcSize, srcSize))
	draw.Draw(src, src.Bounds(), &image.Uniform{color.NRGBA{R: 200, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode source png: %v", err)
	}
	p := New()
	result, err := p.Resize(buf.Bytes(), Options{
		Width:          canvasSize,
		Height:         canvasSize,
		Format:         FormatPNG,
		JPEGQuality:    80,
		WebPQuality:    75,
		AVIFQuality:    45,
		PNGCompression: 6,
	})
	if err != nil {
		t.Fatalf("Resize returned error: %v", err)
	}
	size, err := bimg.NewImage(result).Size()
	if err != nil {
		t.Fatalf("inspect result size: %v", err)
	}
	if size.Width != canvasSize || size.Height != canvasSize {
		t.Fatalf("got %dx%d, want %dx%d", size.Width, size.Height, canvasSize, canvasSize)
	}
	img, err := png.Decode(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("decode result png: %v", err)
	}
	left := (canvasSize - srcSize) / 2
	alpha := color.NRGBAModel.Convert(img.At(0, 0)).(color.NRGBA).A
	if alpha != 0 {
		t.Fatalf("expected transparent padding, got alpha=%d", alpha)
	}
	center := color.NRGBAModel.Convert(img.At(left+srcSize/2, left+srcSize/2)).(color.NRGBA)
	if center.A != 255 || center.R < 180 {
		t.Fatalf("expected solid source pixel at center, got %+v", center)
	}
}

func TestResizeUpscalesWithSingleDimension(t *testing.T) {
	srcWidth := 10
	srcHeight := 6
	src := image.NewNRGBA(image.Rect(0, 0, srcWidth, srcHeight))
	tone := color.NRGBA{R: 120, G: 60, B: 30, A: 255}
	draw.Draw(src, src.Bounds(), &image.Uniform{tone}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode source png: %v", err)
	}

	p := New()
	targetWidth := 20
	result, err := p.Resize(buf.Bytes(), Options{
		Width:          targetWidth,
		Height:         0,
		Format:         FormatJPEG,
		JPEGQuality:    90,
		WebPQuality:    75,
		AVIFQuality:    45,
		PNGCompression: 6,
		EnsureOpaque:   true,
	})
	if err != nil {
		t.Fatalf("Resize returned error: %v", err)
	}

	size, err := bimg.NewImage(result).Size()
	if err != nil {
		t.Fatalf("inspect result size: %v", err)
	}
	expectedHeight := int(float64(srcHeight) * float64(targetWidth) / float64(srcWidth))
	if size.Width != targetWidth || size.Height != expectedHeight {
		t.Fatalf("got %dx%d, want %dx%d", size.Width, size.Height, targetWidth, expectedHeight)
	}

	decoded, _, err := image.Decode(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("decode resized image: %v", err)
	}
	marginX := (size.Width - srcWidth) / 2
	marginY := (size.Height - srcHeight) / 2
	if marginX == 0 || marginY == 0 {
		t.Fatalf("expected positive margins, got marginX=%d marginY=%d", marginX, marginY)
	}
	corner := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA)
	if corner.A != 255 {
		t.Fatalf("expected opaque background, got alpha=%d", corner.A)
	}
	if corner.R < 240 || corner.G < 240 || corner.B < 240 {
		t.Fatalf("expected light background, got %+v", corner)
	}
	centerX := marginX + srcWidth/2
	centerY := marginY + srcHeight/2
	center := color.NRGBAModel.Convert(decoded.At(centerX, centerY)).(color.NRGBA)
	if center.A != 255 {
		t.Fatalf("expected opaque source pixel, got alpha=%d", center.A)
	}
	if diff(center.R, tone.R) > 15 || diff(center.G, tone.G) > 15 || diff(center.B, tone.B) > 15 {
		t.Fatalf("expected source tone at center, got %+v", center)
	}
}

func diff(a, b uint8) uint8 {
	if a > b {
		return a - b
	}
	return b - a
}

func TestResizeDownscaleFitsWithinCanvas(t *testing.T) {
	srcWidth := 12
	srcHeight := 6
	src := image.NewNRGBA(image.Rect(0, 0, srcWidth, srcHeight))
	fill := color.NRGBA{R: 10, G: 200, B: 30, A: 255}
	draw.Draw(src, src.Bounds(), &image.Uniform{fill}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode source png: %v", err)
	}
	p := New()
	target := 6
	result, err := p.Resize(buf.Bytes(), Options{
		Width:          target,
		Height:         target,
		Format:         FormatPNG,
		JPEGQuality:    80,
		WebPQuality:    75,
		AVIFQuality:    45,
		PNGCompression: 6,
	})
	if err != nil {
		t.Fatalf("Resize returned error: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(result))
	if err != nil {
		t.Fatalf("decode resized image: %v", err)
	}
	bounds := decoded.Bounds()
	if bounds.Dx() != target || bounds.Dy() != target {
		t.Fatalf("expected %dx%d, got %dx%d", target, target, bounds.Dx(), bounds.Dy())
	}
	corner := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA)
	if corner.A != 0 {
		t.Fatalf("expected transparent padding at corner, got alpha=%d", corner.A)
	}
	center := color.NRGBAModel.Convert(decoded.At(bounds.Dx()/2, bounds.Dy()/2)).(color.NRGBA)
	if center.A != 255 {
		t.Fatalf("expected opaque center pixel, got alpha=%d", center.A)
	}
	if diff(center.R, fill.R) > 5 || diff(center.G, fill.G) > 5 || diff(center.B, fill.B) > 5 {
		t.Fatalf("expected fill color at center, got %+v", center)
	}
	top := color.NRGBAModel.Convert(decoded.At(bounds.Dx()/2, 0)).(color.NRGBA)
	if top.A != 0 {
		t.Fatalf("expected transparent padding at top edge, got alpha=%d", top.A)
	}
}

// cmykJPEG builds a small CMYK JPEG — the colourspace a print-ready banner
// arrives in when someone uploads it to a CMS block.
func cmykJPEG(t *testing.T, width, height int) []byte {
	t.Helper()

	src := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(src, src.Bounds(), &image.Uniform{color.NRGBA{R: 20, G: 90, B: 160, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode source png: %v", err)
	}

	cmyk, err := bimg.NewImage(buf.Bytes()).Process(bimg.Options{
		Type:           bimg.JPEG,
		Quality:        90,
		Interpretation: bimg.InterpretationCMYK,
	})
	if err != nil {
		t.Skipf("libvips cannot produce a CMYK jpeg here: %v", err)
	}

	interpretation, err := bimg.NewImage(cmyk).Interpretation()
	if err != nil {
		t.Fatalf("read interpretation: %v", err)
	}
	if interpretation != bimg.InterpretationCMYK {
		t.Skipf("libvips produced %v, not CMYK", interpretation)
	}

	return cmyk
}

func TestToSRGBConvertsCMYK(t *testing.T) {
	source := cmykJPEG(t, 600, 300)

	converted := toSRGB(source)
	if converted == nil {
		t.Fatal("expected a CMYK source to be converted, got nil")
	}

	interpretation, err := bimg.NewImage(converted).Interpretation()
	if err != nil {
		t.Fatalf("read converted interpretation: %v", err)
	}
	if interpretation != bimg.InterpretationSRGB {
		t.Fatalf("got interpretation %v, want sRGB", interpretation)
	}

	// And the converted payload still survives the resize pipeline that the
	// raw CMYK one used to fail on ("linear: vector must have 1 or 4 elements").
	result, err := New().Resize(source, Options{
		Width:          382,
		Format:         FormatJPEG,
		JPEGQuality:    80,
		WebPQuality:    75,
		AVIFQuality:    70,
		AVIFSpeed:      8,
		PNGCompression: 6,
		EnsureOpaque:   true,
	})
	if err != nil {
		t.Fatalf("Resize returned error: %v", err)
	}
	size, err := bimg.NewImage(result).Size()
	if err != nil {
		t.Fatalf("inspect result: %v", err)
	}
	if size.Width != 382 {
		t.Fatalf("got width %d, want 382", size.Width)
	}
}

func TestToSRGBLeavesRGBSourceAlone(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(src, src.Bounds(), &image.Uniform{color.NRGBA{R: 10, G: 20, B: 30, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode source png: %v", err)
	}
	if got := toSRGB(buf.Bytes()); got != nil {
		t.Fatalf("expected an sRGB source to be left untouched, got %d bytes back", len(got))
	}
}
