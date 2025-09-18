package processor

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"

	"github.com/h2non/bimg"
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
