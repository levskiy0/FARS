package processor

import (
	"errors"
	"image/color"
	"testing"

	"github.com/h2non/bimg"
)

// TestResizeEmptySourceIsUnsupported keeps a zero-byte original on the 415
// side of the handler's error switch: it is undecodable input, not a service
// failure. It used to return a bare error and surface as a 500.
func TestResizeEmptySourceIsUnsupported(t *testing.T) {
	_, err := New().Resize(nil, Options{Width: 20, Format: FormatPNG, PNGCompression: 6})
	if !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("err = %v, want it to wrap ErrUnsupportedSource", err)
	}
}

// TestResizeChecksDerivedAxisOnDownscale is the unit-level twin of the
// handler case: with only one axis given, the other is derived from the
// source aspect ratio and must face the same cap — the downscale path used
// to skip the check entirely.
func TestResizeChecksDerivedAxisOnDownscale(t *testing.T) {
	tall := solidColorPNG(t, 40, 4000, color.NRGBA{R: 10, G: 20, B: 30, A: 255})

	_, err := New().Resize(tall, Options{
		Width:          20, // derived height: 4000 * 20/40 = 2000
		Format:         FormatPNG,
		PNGCompression: 6,
		MaxWidth:       200,
		MaxHeight:      200,
	})
	if !errors.Is(err, ErrDimensionsTooLarge) {
		t.Fatalf("err = %v, want it to wrap ErrDimensionsTooLarge", err)
	}
}

// TestResizeZeroGeometry pins the meaning of a request with no dimensions at
// all: fit inside the envelope without upscaling, or — with no envelope
// configured — transcode the source at its own size.
func TestResizeZeroGeometry(t *testing.T) {
	source := solidColorPNG(t, 1000, 500, color.NRGBA{R: 200, G: 10, B: 10, A: 255})

	tests := []struct {
		name       string
		opts       Options
		wantWidth  int
		wantHeight int
	}{
		{
			name:       "fits inside the envelope",
			opts:       Options{Format: FormatPNG, PNGCompression: 6, MaxWidth: 200, MaxHeight: 200},
			wantWidth:  200,
			wantHeight: 100,
		},
		{
			name:       "no upscaling when the source already fits",
			opts:       Options{Format: FormatPNG, PNGCompression: 6, MaxWidth: 4000, MaxHeight: 4000},
			wantWidth:  1000,
			wantHeight: 500,
		},
		{
			name:       "no envelope means a plain transcode",
			opts:       Options{Format: FormatPNG, PNGCompression: 6},
			wantWidth:  1000,
			wantHeight: 500,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := New().Resize(source, tc.opts)
			if err != nil {
				t.Fatalf("Resize returned error: %v", err)
			}
			size, err := bimg.NewImage(result).Size()
			if err != nil {
				t.Fatalf("inspect result size: %v", err)
			}
			if size.Width != tc.wantWidth || size.Height != tc.wantHeight {
				t.Fatalf("got %dx%d, want %dx%d", size.Width, size.Height, tc.wantWidth, tc.wantHeight)
			}
		})
	}
}

// TestResizeDegenerateGeometry covers geometries that would shrink an axis
// below one pixel: libvips fails those with "shrunk to nothing", which used
// to reach the client as a 500.
func TestResizeDegenerateGeometry(t *testing.T) {
	tall := solidColorPNG(t, 40, 4000, color.NRGBA{R: 10, G: 20, B: 30, A: 255})

	for _, opts := range []Options{
		{Height: 20, Format: FormatPNG, PNGCompression: 6, MaxWidth: 200, MaxHeight: 200},
		{Width: 200, Height: 20, Format: FormatPNG, PNGCompression: 6, MaxWidth: 200, MaxHeight: 200},
	} {
		_, err := New().Resize(tall, opts)
		if !errors.Is(err, ErrDegenerateGeometry) {
			t.Fatalf("Resize(%dx%d) err = %v, want it to wrap ErrDegenerateGeometry", opts.Width, opts.Height, err)
		}
	}
}
