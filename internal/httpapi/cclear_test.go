package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"fars/internal/cache"
	"fars/internal/config"
	"github.com/gin-gonic/gin"
)

func TestClearOriginalPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name    string
		path    string
		missing bool
	}{
		{name: "nested", path: "img/p/1/2/12.jpg"},
		{name: "unicode and space", path: "img/каталог/фото 1.jpg"},
		{name: "literal percent", path: "img/photo%20.jpg"},
		{name: "deleted original", path: "img/deleted.jpg", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, router := clearTestRouter(t, "secret")
			if !tc.missing {
				writeHTTPTestFile(t, cfg.Storage.BaseDir, tc.path, []byte("original"))
			}
			var variants []string
			for _, rel := range []string{"200x200/" + tc.path, "400x400/" + tc.path + ".webp", "0x100/" + tc.path + ".avif"} {
				variants = append(variants, writeHTTPTestFile(t, cfg.Storage.CacheDir, rel, []byte("cached")))
			}
			other := writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/other.jpg", []byte("unrelated"))
			target := (&url.URL{Path: "/cclear/" + tc.path}).String()
			for attempt := 0; attempt < 2; attempt++ {
				response := callClear(router, http.MethodPost, target, "Bearer secret")
				if response.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				var body struct {
					Invalidated []string `json:"invalidated"`
					Removed     int      `json:"variants_removed"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				wantRemoved := 3
				if attempt == 1 {
					wantRemoved = 0
				}
				if body.Removed != wantRemoved || len(body.Invalidated) != 1 || body.Invalidated[0] != tc.path {
					t.Fatalf("unexpected response: %+v", body)
				}
			}
			for _, path := range variants {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("resize survived: %s (%v)", path, err)
				}
			}
			if data, err := os.ReadFile(other); err != nil || string(data) != "unrelated" {
				t.Fatalf("unrelated variant changed: %q, %v", data, err)
			}
		})
	}
}

func TestClearRejectsUnauthorizedOrInvalidRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, method, path, token, auth string
		status                          int
	}{
		{"disabled", http.MethodPost, "/cclear/img/a.jpg", "", "", http.StatusNotFound},
		{"no auth", http.MethodPost, "/cclear/img/a.jpg", "secret", "", http.StatusUnauthorized},
		{"wrong token", http.MethodPost, "/cclear/img/a.jpg", "secret", "Bearer wrong!", http.StatusUnauthorized},
		{"GET cannot delete", http.MethodGet, "/cclear/img/a.jpg", "secret", "Bearer secret", http.StatusNotFound},
		{"empty", http.MethodPost, "/cclear/", "secret", "Bearer secret", http.StatusBadRequest},
		{"traversal", http.MethodPost, "/cclear/%2e%2e/outside.jpg", "secret", "Bearer secret", http.StatusBadRequest},
		{"nul", http.MethodPost, "/cclear/img/a%00.jpg", "secret", "Bearer secret", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, router := clearTestRouter(t, tc.token)
			variant := writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/a.jpg", []byte("cached"))
			response := callClear(router, tc.method, tc.path, tc.auth)
			if response.Code != tc.status {
				t.Fatalf("status=%d, want=%d, body=%s", response.Code, tc.status, response.Body.String())
			}
			if _, err := os.Stat(variant); err != nil {
				t.Fatalf("rejected request removed resize: %v", err)
			}
		})
	}
}

func TestInvalidationRejectsNULBeforeDeletingAnyPath(t *testing.T) {
	cfg, _, router := clearTestRouter(t, "secret")
	variant := writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/a.jpg", []byte("cached"))
	request := httptest.NewRequest(http.MethodPost, "/cache/invalidate", bytes.NewBufferString(`{"paths":["img/a.jpg","img/b\u0000.jpg"]}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Errorf("status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(variant); err != nil {
		t.Fatalf("partially deleted invalid batch: %v", err)
	}
}

func clearTestRouter(t *testing.T, token string) (*config.Config, *cache.Manager, *gin.Engine) {
	t.Helper()
	cfg := &config.Config{
		Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()},
		Cache:   config.CacheConfig{InvalidationToken: token},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := cache.NewManager(cfg, logger)
	router := gin.New()
	NewHandler(cfg, manager, nil, logger).Register(router)
	return cfg, manager, router
}

func callClear(router http.Handler, method, path, auth string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", auth)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestClearAppliesRewritesAndRejectsAbsoluteTargets(t *testing.T) {
	for _, tc := range []struct {
		name, replacement string
		status            int
	}{
		{"relative target", "img/a.jpg", http.StatusOK},
		{"absolute target", "/outside.jpg", http.StatusBadRequest},
		{"escaping target", "../outside.jpg", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, router := clearTestRouter(t, "secret")
			raw := fmt.Sprintf("storage:\n  base_dir: %q\n  cache_dir: %q\nrewrites:\n  - pattern: '^alias[.]jpg$'\n    replacement: %q\n",
				cfg.Storage.BaseDir, cfg.Storage.CacheDir, tc.replacement)
			loaded, err := config.LoadReader(strings.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Rewrites = loaded.Rewrites
			variant := writeHTTPTestFile(t, cfg.Storage.CacheDir, "200x200/img/a.jpg", []byte("resize"))
			r := callClear(router, http.MethodPost, "/cclear/alias.jpg", "Bearer secret")
			if r.Code != tc.status {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
			_, err = os.Stat(variant)
			if tc.status == http.StatusOK && !os.IsNotExist(err) {
				t.Fatalf("rewrite did not remove variant: %v", err)
			}
			if tc.status != http.StatusOK && err != nil {
				t.Fatalf("invalid target removed variant: %v", err)
			}
		})
	}
}
