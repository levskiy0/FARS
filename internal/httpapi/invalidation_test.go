package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"fars/internal/cache"
	"fars/internal/config"
)

func TestManualInvalidationRequiresTokenAndRemovesVariants(t *testing.T) {
	gin.SetMode(gin.TestMode)
	baseDir := t.TempDir()
	cacheDir := t.TempDir()
	originalRel := "img/photo.jpg"
	writeHTTPTestFile(t, baseDir, originalRel, []byte("original"))
	variantA := writeHTTPTestFile(t, cacheDir, "200x200/img/photo.jpg", []byte("a"))
	variantB := writeHTTPTestFile(t, cacheDir, "400x400/img/photo.jpg.webp", []byte("b"))

	cfg := &config.Config{
		Storage: config.StorageConfig{BaseDir: baseDir, CacheDir: cacheDir},
		Cache:   config.CacheConfig{InvalidationToken: "secret"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := &Handler{cfg: cfg, cache: cache.NewManager(cfg, logger), logger: logger}
	router := gin.New()
	handler.Register(router)

	unauthorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/cache/invalidate", bytes.NewBufferString(`{"paths":["img/photo.jpg"]}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected unauthorized status: %d", unauthorized.Code)
	}

	response := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/cache/invalidate", bytes.NewBufferString(`{"paths":["img/photo.jpg"]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer secret")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected invalidation status: %d, body=%s", response.Code, response.Body.String())
	}
	for _, path := range []string{variantA, variantB} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected variant %s to be removed, stat error: %v", path, err)
		}
	}
}

func TestManualInvalidationRouteDisabledWithoutToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{Cache: config.CacheConfig{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := &Handler{cfg: cfg, logger: logger}
	router := gin.New()
	handler.Register(router)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/cache/invalidate", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("expected disabled route to return 404, got %d", response.Code)
	}
}

func writeHTTPTestFile(t *testing.T, root, relative string, payload []byte) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
