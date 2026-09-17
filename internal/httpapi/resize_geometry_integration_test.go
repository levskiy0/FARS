//go:build integration

package httpapi

import (
	"image/color"
	"net/http"
	"net/http/httptest"
	"testing"

	"fars/internal/config"
	"github.com/h2non/bimg"
)

// TestHandleResizeSingleAxisRespectsCaps covers the axis the client does not
// name. Only the explicit one is checked up front, so a downscale request
// with a zero axis used to slip past the envelope entirely: with caps of
// 200x200 and a 40x4000 source, /resize/20x0 produced a 20x2000 image and
// cached it.
func TestHandleResizeSingleAxisRespectsCaps(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		srcWidth   int
		srcHeight  int
		geometry   string
		wantStatus int
		wantWidth  int
		wantHeight int
	}{
		{
			name:       "derived height above cap on a downscale",
			source:     "img/tall.png",
			srcWidth:   40,
			srcHeight:  4000,
			geometry:   "20x0",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "derived width above cap on a downscale",
			source:     "img/wide.png",
			srcWidth:   4000,
			srcHeight:  40,
			geometry:   "0x20",
			wantStatus: http.StatusBadRequest,
		},
		{
			// The mirror geometry on the same source: the free axis scales to
			// a fifth of a pixel. libvips fails that with "shrunk to nothing",
			// which surfaced as a 500 for a purely client-chosen geometry.
			name:       "sub-pixel derived width is a client error, not a 500",
			source:     "img/tall.png",
			srcWidth:   40,
			srcHeight:  4000,
			geometry:   "0x20",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "sub-pixel derived height is a client error, not a 500",
			source:     "img/wide.png",
			srcWidth:   4000,
			srcHeight:  40,
			geometry:   "20x0",
			wantStatus: http.StatusBadRequest,
		},
		{
			// Same collapse reached through the both-axes path: 200x20 of a
			// 40x4000 source puts the content at 0.2px wide.
			name:       "sub-pixel content on the both-axes path",
			source:     "img/tall.png",
			srcWidth:   40,
			srcHeight:  4000,
			geometry:   "200x20",
			wantStatus: http.StatusBadRequest,
		},
		{
			// The guarded upscale branch still behaves: 200x0 of the same
			// source derives a 20000px height, far past the cap.
			name:       "derived height above cap on an upscale",
			source:     "img/tall.png",
			srcWidth:   40,
			srcHeight:  4000,
			geometry:   "200x0",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _, router := newResizeTestServer(t, config.ResizeConfig{MaxWidth: 200, MaxHeight: 200})
			writeHTTPTestFile(t, cfg.Storage.BaseDir, tc.source,
				pngOfSize(t, tc.srcWidth, tc.srcHeight, color.NRGBA{R: 30, G: 60, B: 90, A: 255}))

			request := httptest.NewRequest(http.MethodGet, "/resize/"+tc.geometry+"/"+tc.source, nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != tc.wantStatus {
				t.Fatalf("status=%d, want=%d, body=%s", response.Code, tc.wantStatus, response.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				if n := countRegularFiles(t, cfg.Storage.CacheDir); n != 0 {
					t.Fatalf("a rejected geometry must not be cached, found %d files", n)
				}
				return
			}
			size, err := bimg.NewImage(response.Body.Bytes()).Size()
			if err != nil {
				t.Fatalf("inspect result size: %v", err)
			}
			if size.Width != tc.wantWidth || size.Height != tc.wantHeight {
				t.Fatalf("got %dx%d, want %dx%d", size.Width, size.Height, tc.wantWidth, tc.wantHeight)
			}
		})
	}
}

// TestHandleResizeZeroGeometryFitsEnvelope pins the meaning of the geometry
// the PrestaShop module emits when it knows no dimensions ("0x0", and the
// equivalent bare "x"): fit inside resize.max_* keeping the aspect ratio,
// never upscaling. It used to be a 400, which would have broken every such
// storefront URL.
func TestHandleResizeZeroGeometryFitsEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		geometry   string
		srcWidth   int
		srcHeight  int
		wantWidth  int
		wantHeight int
	}{
		{name: "landscape source is fitted", geometry: "0x0", srcWidth: 1000, srcHeight: 500, wantWidth: 200, wantHeight: 100},
		{name: "portrait source is fitted", geometry: "0x0", srcWidth: 500, srcHeight: 1000, wantWidth: 100, wantHeight: 200},
		{name: "small source is not upscaled", geometry: "0x0", srcWidth: 40, srcHeight: 30, wantWidth: 40, wantHeight: 30},
		{name: "bare x means the same thing", geometry: "x", srcWidth: 1000, srcHeight: 500, wantWidth: 200, wantHeight: 100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _, router := newResizeTestServer(t, config.ResizeConfig{MaxWidth: 200, MaxHeight: 200})
			writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png",
				pngOfSize(t, tc.srcWidth, tc.srcHeight, color.NRGBA{R: 200, G: 30, B: 30, A: 255}))

			request := httptest.NewRequest(http.MethodGet, "/resize/"+tc.geometry+"/img/photo.png", nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			size, err := bimg.NewImage(response.Body.Bytes()).Size()
			if err != nil {
				t.Fatalf("inspect result size: %v", err)
			}
			if size.Width != tc.wantWidth || size.Height != tc.wantHeight {
				t.Fatalf("got %dx%d, want %dx%d", size.Width, size.Height, tc.wantWidth, tc.wantHeight)
			}
			if n := countRegularFiles(t, cfg.Storage.CacheDir); n != 1 {
				t.Fatalf("expected exactly one cache file, found %d", n)
			}
		})
	}
}
