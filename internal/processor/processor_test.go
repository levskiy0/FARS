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
