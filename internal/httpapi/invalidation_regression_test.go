package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/processor"
	"github.com/gin-gonic/gin"
)

func TestInvalidationValidatesWholeBatchBeforeDeleting(t *testing.T) {
	cfg := &config.Config{Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()}, Cache: config.CacheConfig{InvalidationToken: "secret"}}
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/a.jpg", []byte("original"))
	path := writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/a.jpg", []byte("cached"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(cfg, cache.NewManager(cfg, logger), nil, logger)
	router := gin.New()
	h.Register(router)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/cache/invalidate", bytes.NewBufferString(`{"paths":["img/a.jpg","../outside.jpg"]}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("partial destructive batch: %v", err)
	}
}

func TestMismatchedETagOverridesIfModifiedSince(t *testing.T) {
	cfg := &config.Config{Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()}}
	originalPath := writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/a.jpg", []byte("original"))
	originalInfo, err := os.Stat(originalPath)
	if err != nil {
		t.Fatalf("stat original: %v", err)
	}
	path := writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/a.jpg", []byte("new image"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(cfg, cache.NewManager(cfg, logger), nil, logger)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodGet, "/resize/200x200/img/a.jpg", nil)
	c.Request.Header.Set("If-None-Match", `"old-etag"`)
	c.Request.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).Format(http.TimeFormat))
	// tryServeFromCache now keys Last-Modified off the original's mtime, not
	// the cache file's own, so it needs a real originalInfo (see fix for the
	// Last-Modified inconsistency between a cache hit and a fresh render).
	if !h.tryServeFromCache(c, path, processor.FormatJPEG, originalInfo) {
		t.Fatal("cache miss")
	}
	if response.Code != http.StatusOK || response.Body.String() != "new image" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
