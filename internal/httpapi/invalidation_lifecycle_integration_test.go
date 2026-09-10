//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/processor"
	"github.com/gin-gonic/gin"
)

// Exercises the real encoder, disk cache, startup job and on-demand regeneration.
// All data is generated in temporary directories; no production images are touched.
func TestResizeInvalidationLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"scanner", "POST"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{
				Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()},
				Resize:  config.ResizeConfig{MaxWidth: 100, MaxHeight: 100, JPGQuality: 85, WebPQuality: 85},
				Cache:   config.CacheConfig{InvalidationToken: "secret", CheckOriginalsInterval: config.Duration{Duration: 10 * time.Millisecond}},
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			manager := cache.NewManager(cfg, logger)
			router := gin.New()
			NewHandler(cfg, manager, processor.New(), logger).Register(router)
			source := writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png", solidPNG(t, color.NRGBA{R: 255, A: 255}))
			writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/other.png", solidPNG(t, color.NRGBA{G: 255, A: 255}))
			paths := []string{"8x8/img/photo.png", "16x16/img/photo.png.webp", "x8/img/photo.png"}
			oldResponses := make(map[string][]byte)
			oldETags := make(map[string]string)
			for _, path := range paths {
				r := callClear(router, http.MethodGet, "/resize/"+path, "")
				if r.Code != http.StatusOK {
					t.Fatalf("resize %s: %d %s", path, r.Code, r.Body.String())
				}
				oldResponses[path] = append([]byte(nil), r.Body.Bytes()...)
				oldETags[path] = r.Header().Get("ETag")
				if data, err := os.ReadFile(filepath.Join(cfg.Storage.CacheDir, path)); err != nil || !bytes.Equal(data, r.Body.Bytes()) {
					t.Fatalf("resize not published: %v", err)
				}
			}
			other := callClear(router, http.MethodGet, "/resize/8x8/img/other.png", "")
			if other.Code != http.StatusOK {
				t.Fatalf("other resize: %d", other.Code)
			}

			ctx, cancel := context.WithCancel(context.Background())
			if mode == "scanner" {
				manager.StartBackground(ctx)
			}
			t.Cleanup(func() {
				cancel()
				stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := manager.WaitBackground(stopCtx); err != nil {
					t.Errorf("shutdown: %v", err)
				}
			})
			replacement := writeHTTPTestFile(t, cfg.Storage.BaseDir, "replacement.png", solidPNG(t, color.NRGBA{B: 255, A: 255}))
			if err := os.Rename(replacement, source); err != nil {
				t.Fatal(err)
			}
			if mode == "POST" {
				r := callClear(router, http.MethodPost, "/cclear/img/photo.png", "Bearer secret")
				if r.Code != http.StatusOK {
					t.Fatalf("clear: %d %s", r.Code, r.Body.String())
				}
			}
			// No resize requests while waiting: only the scanner/POST can remove files.
			waitForResizeRemoval(t, cfg.Storage.CacheDir, paths)
			otherDisk, err := os.ReadFile(filepath.Join(cfg.Storage.CacheDir, "8x8/img/other.png"))
			if err != nil || !bytes.Equal(otherDisk, other.Body.Bytes()) {
				t.Fatalf("other resize changed: %v", err)
			}
			for _, path := range paths {
				request := httptest.NewRequest(http.MethodGet, "/resize/"+path, nil)
				request.Header.Set("If-None-Match", oldETags[path])
				r := httptest.NewRecorder()
				router.ServeHTTP(r, request)
				if r.Code != http.StatusOK || bytes.Equal(oldResponses[path], r.Body.Bytes()) || r.Header().Get("ETag") == oldETags[path] {
					t.Fatalf("stale response after invalidation %s: status=%d", path, r.Code)
				}
				data, err := os.ReadFile(filepath.Join(cfg.Storage.CacheDir, path))
				if err != nil || !bytes.Equal(data, r.Body.Bytes()) {
					t.Fatalf("new resize not cached: %v", err)
				}
			}
		})
	}
}

func solidPNG(t *testing.T, fill color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func waitForResizeRemoval(t *testing.T, root string, paths []string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		missing := 0
		for _, path := range paths {
			_, err := os.Stat(filepath.Join(root, path))
			if errors.Is(err, os.ErrNotExist) {
				missing++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		if missing == len(paths) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("resizes still exist after invalidation")
		case <-ticker.C:
		}
	}
}
