//go:build integration

package httpapi

import (
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"fars/internal/config"
	"github.com/h2non/bimg"
)

// TestHandleResizeErrorPaths drives every documented failure through the
// real router (real processor, real disk cache) and asserts both the status
// code and that the failure left no cache file behind.
func TestHandleResizeErrorPaths(t *testing.T) {
	tests := []struct {
		name       string
		wantStatus int
		setup      func(t *testing.T, cfg *config.Config) string // returns the request path
	}{
		{
			// 0x0 is a valid request now (see
			// TestHandleResizeZeroGeometryFitsEnvelope) — the PrestaShop
			// module emits it — so the only thing wrong here is the missing
			// original.
			name:       "zero by zero geometry on a missing original",
			wantStatus: http.StatusNotFound,
			setup: func(t *testing.T, cfg *config.Config) string {
				return "/resize/0x0/img/whatever.jpg"
			},
		},
		{
			name:       "geometry above caps outright",
			wantStatus: http.StatusBadRequest,
			setup: func(t *testing.T, cfg *config.Config) string {
				return "/resize/5000x5000/img/whatever.jpg"
			},
		},
		{
			// caps 200x200, a 99x200 source, request 100x0: the derived
			// height (100 * 200/99 ≈ 202) just clears MaxHeight, while the
			// resulting canvas area (100*202=20200) stays comfortably under
			// the 200x200 pixel budget — isolating the axis-cap check from
			// the separate whole-canvas pixel-budget guard.
			name:       "derived axis exceeds cap",
			wantStatus: http.StatusBadRequest,
			setup: func(t *testing.T, cfg *config.Config) string {
				writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/tall.png", pngOfSize(t, 99, 200, color.NRGBA{R: 10, G: 10, B: 10, A: 255}))
				return "/resize/100x0/img/tall.png"
			},
		},
		{
			name:       "NUL byte in path",
			wantStatus: http.StatusBadRequest,
			setup: func(t *testing.T, cfg *config.Config) string {
				return "/resize/200x200/img/a%00.jpg"
			},
		},
		{
			name:       "path is a directory",
			wantStatus: http.StatusNotFound,
			setup: func(t *testing.T, cfg *config.Config) string {
				dir := filepath.Join(cfg.Storage.BaseDir, "img", "adir.jpg")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
				return "/resize/200x200/img/adir.jpg"
			},
		},
		{
			name:       "missing file",
			wantStatus: http.StatusNotFound,
			setup: func(t *testing.T, cfg *config.Config) string {
				return "/resize/200x200/img/missing.jpg"
			},
		},
		{
			// Undecodable input, like the text-bytes case below, not a
			// service failure.
			name:       "zero byte file",
			wantStatus: http.StatusUnsupportedMediaType,
			setup: func(t *testing.T, cfg *config.Config) string {
				writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/empty.jpg", []byte{})
				return "/resize/200x200/img/empty.jpg"
			},
		},
		{
			name:       "text bytes with image extension",
			wantStatus: http.StatusUnsupportedMediaType,
			setup: func(t *testing.T, cfg *config.Config) string {
				writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/text.jpg", []byte("this is definitely not an image"))
				return "/resize/200x200/img/text.jpg"
			},
		},
		{
			name:       "unsupported extension",
			wantStatus: http.StatusUnsupportedMediaType,
			setup: func(t *testing.T, cfg *config.Config) string {
				writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.bmp", []byte("irrelevant bytes"))
				return "/resize/200x200/img/photo.bmp"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _, router := newResizeTestServer(t, config.ResizeConfig{MaxWidth: 200, MaxHeight: 200})
			path := tc.setup(t, cfg)

			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != tc.wantStatus {
				t.Fatalf("status=%d, want=%d, body=%s", response.Code, tc.wantStatus, response.Body.String())
			}
			if n := countRegularFiles(t, cfg.Storage.CacheDir); n != 0 {
				t.Fatalf("expected no cache file written for a failing request, found %d", n)
			}
		})
	}
}

// TestHandleResizeDerivedAxisExactlyAtCapSucceeds is the positive twin of the
// "derived axis exceeds cap" case above: landing exactly on the configured
// cap must succeed with the expected pixel size, not be rejected.
func TestHandleResizeDerivedAxisExactlyAtCapSucceeds(t *testing.T) {
	cfg, _, _, router := newResizeTestServer(t, config.ResizeConfig{MaxWidth: 200, MaxHeight: 200})
	// 4x8 source, requested width 100 => derived height 100*8/4=200, exactly MaxHeight.
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/thin.png", pngOfSize(t, 4, 8, color.NRGBA{R: 20, G: 40, B: 200, A: 255}))

	request := httptest.NewRequest(http.MethodGet, "/resize/100x0/img/thin.png", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	size, err := bimg.NewImage(response.Body.Bytes()).Size()
	if err != nil {
		t.Fatalf("inspect result size: %v", err)
	}
	if size.Width != 100 || size.Height != 200 {
		t.Fatalf("got %dx%d, want 100x200 (derived height landing exactly on the cap)", size.Width, size.Height)
	}
	if n := countRegularFiles(t, cfg.Storage.CacheDir); n != 1 {
		t.Fatalf("expected exactly one cache file to be written, found %d", n)
	}
}
