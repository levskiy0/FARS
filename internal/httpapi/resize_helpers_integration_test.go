//go:build integration

package httpapi

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"testing"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/processor"
	"github.com/gin-gonic/gin"
)

// newResizeTestServer wires a real Handler (real processor, real disk cache)
// behind a gin engine, the way the process does in production. resize lets
// each test set its own caps/quality; zero fields fall back to generous
// defaults so callers only need to specify what the test cares about.
func newResizeTestServer(t *testing.T, resize config.ResizeConfig) (*config.Config, *cache.Manager, *Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	if resize.MaxWidth == 0 {
		resize.MaxWidth = 2000
	}
	if resize.MaxHeight == 0 {
		resize.MaxHeight = 2000
	}
	if resize.JPGQuality == 0 {
		resize.JPGQuality = 85
	}
	if resize.WebPQuality == 0 {
		resize.WebPQuality = 80
	}
	if resize.AVIFQuality == 0 {
		resize.AVIFQuality = 60
	}
	if resize.AVIFSpeed == 0 {
		resize.AVIFSpeed = 8
	}
	if resize.PNGCompression == 0 {
		resize.PNGCompression = 6
	}

	cfg := &config.Config{
		Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()},
		Resize:  resize,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := cache.NewManager(cfg, logger)
	handler := NewHandler(cfg, manager, processor.New(), logger)
	router := gin.New()
	handler.Register(router)
	return cfg, manager, handler, router
}

// pngOfSize builds a solid-fill PNG of an arbitrary size, for tests that need
// control over the source aspect ratio (e.g. deriving a capped axis).
func pngOfSize(t *testing.T, width, height int, fill color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: fill}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// countRegularFiles walks root and counts plain files, used to assert a
// failing /resize request left no partial or accidental cache entry behind.
func countRegularFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return count
}
