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
)

// TestHandleResizeRefusesSymlinksOutOfBaseDir covers the containment hole
// ResolvePaths cannot close on its own: it is purely lexical, and os.Stat
// follows symlinks, so a link inside base_dir used to serve — and cache —
// a file from anywhere on the filesystem. Originals are opened through an
// os.Root anchored at base_dir instead, which refuses any component that
// leaves it.
func TestHandleResizeRefusesSymlinksOutOfBaseDir(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, cfg *config.Config, outside string) string
		wantStatus int
	}{
		{
			name: "file symlink pointing outside",
			setup: func(t *testing.T, cfg *config.Config, outside string) string {
				target := filepath.Join(outside, "secret.png")
				writeFileAt(t, target, pngOfSize(t, 40, 40, color.NRGBA{R: 1, G: 2, B: 3, A: 255}))
				link := filepath.Join(cfg.Storage.BaseDir, "leak.png")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return "/resize/20x20/leak.png"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "directory symlink pointing outside",
			setup: func(t *testing.T, cfg *config.Config, outside string) string {
				target := filepath.Join(outside, "secret.png")
				writeFileAt(t, target, pngOfSize(t, 40, 40, color.NRGBA{R: 4, G: 5, B: 6, A: 255}))
				link := filepath.Join(cfg.Storage.BaseDir, "elsewhere")
				if err := os.Symlink(outside, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return "/resize/20x20/elsewhere/secret.png"
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "traversal out and back in",
			setup: func(t *testing.T, cfg *config.Config, outside string) string {
				writeFileAt(t, filepath.Join(outside, "secret.png"), pngOfSize(t, 40, 40, color.NRGBA{A: 255}))
				return "/resize/20x20/..%2f" + filepath.Base(outside) + "/secret.png"
			},
			// Caught lexically by ResolvePaths, before the root ever sees it.
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "plain file inside base_dir is unaffected",
			setup: func(t *testing.T, cfg *config.Config, outside string) string {
				writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png", pngOfSize(t, 40, 40, color.NRGBA{R: 7, G: 8, B: 9, A: 255}))
				return "/resize/20x20/img/photo.png"
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "symlink that stays inside base_dir still resolves",
			setup: func(t *testing.T, cfg *config.Config, outside string) string {
				writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png", pngOfSize(t, 40, 40, color.NRGBA{R: 7, G: 8, B: 9, A: 255}))
				link := filepath.Join(cfg.Storage.BaseDir, "alias.png")
				if err := os.Symlink(filepath.Join("img", "photo.png"), link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return "/resize/20x20/alias.png"
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _, router := newResizeTestServer(t, config.ResizeConfig{})
			outside := t.TempDir()
			path := tc.setup(t, cfg, outside)

			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != tc.wantStatus {
				t.Fatalf("status=%d, want=%d, body=%s", response.Code, tc.wantStatus, response.Body.String())
			}
			wantCached := 0
			if tc.wantStatus == http.StatusOK {
				wantCached = 1
			}
			if n := countRegularFiles(t, cfg.Storage.CacheDir); n != wantCached {
				t.Fatalf("cache files = %d, want %d", n, wantCached)
			}
		})
	}
}

func writeFileAt(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
