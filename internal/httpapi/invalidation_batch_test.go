package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"fars/internal/cache"
	"fars/internal/config"
	"github.com/gin-gonic/gin"
)

// TestManualInvalidationRejectsWrongToken extends the existing "missing
// token" and route-disabled coverage (TestManualInvalidationRequiresTokenAndRemovesVariants,
// TestManualInvalidationRouteDisabledWithoutToken) with the "wrong token"
// case, which neither of them exercises for /cache/invalidate.
func TestManualInvalidationRejectsWrongToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()},
		Cache:   config.CacheConfig{InvalidationToken: "secret"},
	}
	variant := writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/a.jpg", []byte("cached"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(cfg, cache.NewManager(cfg, logger), nil, logger)
	router := gin.New()
	handler.Register(router)

	request := httptest.NewRequest(http.MethodPost, "/cache/invalidate", bytes.NewBufferString(`{"paths":["img/a.jpg"]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer wrong-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401, body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(variant); err != nil {
		t.Fatalf("unauthorized request removed a variant: %v", err)
	}
}

// TestManualInvalidationBatchSuccessShape checks the response shape of a
// successful multi-path batch: "invalidated" lists every requested path and
// "variants_removed" is the total count across all of them.
func TestManualInvalidationBatchSuccessShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()},
		Cache:   config.CacheConfig{InvalidationToken: "secret"},
	}
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/a.jpg", []byte("original a"))
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/b.jpg", []byte("original b"))
	writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/a.jpg", []byte("a"))
	writeHTTPTestFile(t, cfg.Storage.CacheDir, "400x400/img/a.jpg.webp", []byte("a-webp"))
	writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/b.jpg", []byte("b"))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(cfg, cache.NewManager(cfg, logger), nil, logger)
	router := gin.New()
	handler.Register(router)

	request := httptest.NewRequest(http.MethodPost, "/cache/invalidate", bytes.NewBufferString(`{"paths":["img/a.jpg","img/b.jpg"]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Invalidated []string `json:"invalidated"`
		Removed     int      `json:"variants_removed"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantInvalidated := []string{"img/a.jpg", "img/b.jpg"}
	if len(body.Invalidated) != len(wantInvalidated) {
		t.Fatalf("invalidated=%v, want %v", body.Invalidated, wantInvalidated)
	}
	for i, path := range wantInvalidated {
		if body.Invalidated[i] != path {
			t.Fatalf("invalidated[%d]=%q, want %q", i, body.Invalidated[i], path)
		}
	}
	if body.Removed != 3 {
		t.Fatalf("variants_removed=%d, want 3", body.Removed)
	}
}

// TestManualInvalidationMidBatchFailureReportsEarlierSuccesses makes deletion
// fail for the second of three paths (by stripping write permission on its
// cache geometry directory, so os.Remove fails on that variant) and checks
// the paths already invalidated before the failure are still reported back,
// alongside the count already removed and the path that failed.
func TestManualInvalidationMidBatchFailureReportsEarlierSuccesses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()},
		Cache:   config.CacheConfig{InvalidationToken: "secret"},
	}
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/a.jpg", []byte("original a"))
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/b.jpg", []byte("original b"))
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/c.jpg", []byte("original c"))
	writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/a.jpg", []byte("a"))
	writeHTTPTestFile(t, cfg.Storage.CacheDir, "999x999/img/b.jpg", []byte("b"))
	writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/c.jpg", []byte("c"))

	// Deny write access to the directory that directly holds b's variant so
	// os.Remove fails on it with a permission error (removing a file needs
	// write access to its parent directory, not the file itself), while a's
	// and c's directories stay writable.
	blockedDir := filepath.Join(cfg.Storage.CacheDir, "999x999", "img")
	if err := os.Chmod(blockedDir, 0o555); err != nil {
		t.Fatalf("chmod %s: %v", blockedDir, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(blockedDir, 0o755); err != nil {
			t.Errorf("restore permissions on %s: %v", blockedDir, err)
		}
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(cfg, cache.NewManager(cfg, logger), nil, logger)
	router := gin.New()
	handler.Register(router)

	request := httptest.NewRequest(http.MethodPost, "/cache/invalidate", bytes.NewBufferString(`{"paths":["img/a.jpg","img/b.jpg","img/c.jpg"]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500, body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Error       string   `json:"error"`
		Path        string   `json:"path"`
		Invalidated []string `json:"invalidated"`
		Removed     int      `json:"variants_removed"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Path != "img/b.jpg" {
		t.Fatalf("failing path=%q, want %q", body.Path, "img/b.jpg")
	}
	if len(body.Invalidated) != 1 || body.Invalidated[0] != "img/a.jpg" {
		t.Fatalf("invalidated=%v, want [img/a.jpg] (the batch stops at the first failure)", body.Invalidated)
	}
	if body.Removed != 1 {
		t.Fatalf("variants_removed=%d, want 1 (only a's variant)", body.Removed)
	}

	// a's variant is really gone; c's was never attempted (batch stops at
	// the first failure) so it must still be there; b's survives because
	// its directory couldn't be written to.
	if _, err := os.Stat(filepath.Join(cfg.Storage.CacheDir, "200x200", "img", "a.jpg")); !os.IsNotExist(err) {
		t.Fatalf("a's variant should have been removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blockedDir, "b.jpg")); err != nil {
		t.Fatalf("b's variant should have survived the permission error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.CacheDir, "200x200", "img", "c.jpg")); err != nil {
		t.Fatalf("c's variant should not have been touched: %v", err)
	}
}
