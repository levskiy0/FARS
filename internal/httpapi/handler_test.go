package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"log/slog"

	"github.com/gin-gonic/gin"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/locker"
	"fars/internal/processor"
	"fars/internal/version"
)

func TestBuildSourceCandidates(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		rawExt string
		want   []sourceCandidate
	}{
		{
			name:   "single extension",
			input:  "test/13.jpg",
			rawExt: ".jpg",
			want: []sourceCandidate{{
				relative:    "test/13.jpg",
				cacheSuffix: "",
			}},
		},
		{
			name:   "double extension to png",
			input:  "test/13.jpg.png",
			rawExt: ".png",
			want: []sourceCandidate{
				{relative: "test/13.jpg.png", cacheSuffix: ""},
				{relative: "test/13.jpg", cacheSuffix: ".png"},
			},
		},
		{
			name:   "uppercase avif",
			input:  "foo/BAR.PNG.AVIF",
			rawExt: ".AVIF",
			want: []sourceCandidate{
				{relative: "foo/BAR.PNG.AVIF", cacheSuffix: ""},
				{relative: "foo/BAR.PNG", cacheSuffix: ".avif"},
			},
		},
		{
			name:   "unsupported base",
			input:  "foo.gif.webp",
			rawExt: ".webp",
			want: []sourceCandidate{{
				relative:    "foo.gif.webp",
				cacheSuffix: "",
			}},
		},
		{
			name:   "no extension",
			input:  "img/item/13",
			rawExt: "",
			want: []sourceCandidate{{
				relative:    "img/item/13",
				cacheSuffix: "",
			}},
		},
	}

	for i := range tests {
		caseData := tests[i]
		t.Run(caseData.name, func(t *testing.T) {
			got := buildSourceCandidates(caseData.input, caseData.rawExt)
			if !reflect.DeepEqual(got, caseData.want) {
				t.Fatalf("buildSourceCandidates(%q, %q) = %#v, want %#v", caseData.input, caseData.rawExt, got, caseData.want)
			}
		})
	}
}

func TestParseGeometry(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		width     int
		height    int
		expectErr bool
	}{
		{
			name:   "both provided",
			input:  "200x300",
			width:  200,
			height: 300,
		},
		{
			name:   "missing height",
			input:  "120x",
			width:  120,
			height: 0,
		},
		{
			name:   "missing width",
			input:  "x480",
			width:  0,
			height: 480,
		},
		{
			name:      "invalid width",
			input:     "axb",
			expectErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			w, h, err := parseGeometry(tc.input)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w != tc.width || h != tc.height {
				t.Fatalf("parseGeometry(%q) = (%d,%d), want (%d,%d)", tc.input, w, h, tc.width, tc.height)
			}
		})
	}
}

func TestTryServeFromCacheHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	baseDir := t.TempDir()
	cacheDir := t.TempDir()

	origPath := filepath.Join(baseDir, "img", "photo.jpg")
	cachePath := filepath.Join(cacheDir, "200x200", "img", "photo.jpg")
	if err := os.MkdirAll(filepath.Dir(origPath), 0o755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	originalPayload := []byte("original-content")
	if err := os.WriteFile(origPath, originalPayload, 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	cachedPayload := []byte("cached-payload")
	if err := os.WriteFile(cachePath, cachedPayload, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	cfg := &config.Config{
		Storage: config.StorageConfig{BaseDir: baseDir, CacheDir: cacheDir},
		Resize:  config.ResizeConfig{MaxWidth: 2000, MaxHeight: 2000},
		Cache:   config.CacheConfig{TTL: config.Duration{Duration: time.Hour}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := &Handler{
		cfg:    cfg,
		cache:  cache.NewManager(cfg, logger),
		locks:  locker.New(),
		logger: logger,
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/resize/200x200/img/photo.jpg", nil)

	originalInfo, err := os.Stat(origPath)
	if err != nil {
		t.Fatalf("stat original: %v", err)
	}
	if !handler.tryServeFromCache(c, cachePath, processor.FormatJPEG, originalInfo) {
		t.Fatalf("expected cache serve to succeed")
	}

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != cacheControlImmutable {
		t.Fatalf("unexpected Cache-Control: %q", got)
	}
	sum := sha256.Sum256(cachedPayload)
	expectedETag := "\"" + hex.EncodeToString(sum[:]) + "\""
	if got := recorder.Header().Get("ETag"); got != expectedETag {
		t.Fatalf("unexpected ETag: %q", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(len(cachedPayload)) {
		t.Fatalf("unexpected Content-Length: %q", got)
	}
	if body := recorder.Body.Bytes(); !reflect.DeepEqual(body, cachedPayload) {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestTryServeFromCacheConditional(t *testing.T) {
	gin.SetMode(gin.TestMode)
	baseDir := t.TempDir()
	cacheDir := t.TempDir()

	origPath := filepath.Join(baseDir, "img", "photo.jpg")
	cachePath := filepath.Join(cacheDir, "200x200", "img", "photo.jpg")
	if err := os.MkdirAll(filepath.Dir(origPath), 0o755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.WriteFile(origPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}
	cachedPayload := []byte("cached-payload")
	if err := os.WriteFile(cachePath, cachedPayload, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	cfg := &config.Config{
		Storage: config.StorageConfig{BaseDir: baseDir, CacheDir: cacheDir},
		Resize:  config.ResizeConfig{MaxWidth: 2000, MaxHeight: 2000},
		Cache:   config.CacheConfig{TTL: config.Duration{Duration: time.Hour}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := &Handler{
		cfg:    cfg,
		cache:  cache.NewManager(cfg, logger),
		locks:  locker.New(),
		logger: logger,
	}

	originalInfo, err := os.Stat(origPath)
	if err != nil {
		t.Fatalf("stat original: %v", err)
	}
	sum := sha256.Sum256(cachedPayload)
	etag := "\"" + hex.EncodeToString(sum[:]) + "\""

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/resize/200x200/img/photo.jpg", nil)
	req.Header.Set("If-None-Match", etag)
	c.Request = req
	if !matchETag(req.Header.Get("If-None-Match"), etag) {
		t.Fatalf("matchETag failed for header %q", req.Header.Get("If-None-Match"))
	}
	if !matchETag(c.GetHeader("If-None-Match"), etag) {
		t.Fatalf("context header missing: %q", c.GetHeader("If-None-Match"))
	}

	if !handler.tryServeFromCache(c, cachePath, processor.FormatJPEG, originalInfo) {
		t.Fatalf("expected cache serve to succeed")
	}
	c.Writer.WriteHeaderNow()

	if recorder.Code != http.StatusNotModified {
		t.Fatalf("unexpected status: %d (etag header %q, expected %q)", recorder.Code, recorder.Header().Get("ETag"), etag)
	}
	if got := recorder.Header().Get("ETag"); got != etag {
		t.Fatalf("unexpected ETag: %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != cacheControlImmutable {
		t.Fatalf("unexpected Cache-Control: %q", got)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected empty body, got %q", recorder.Body.String())
	}
}

func TestRespondErrorHTML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/resize/200x200/img/photo.jpg", nil)

	version.Override("test-version")
	expectedBody := fmt.Sprintf("<html><head><title>404 Not Found</title></head>\n<body>\n<center><h1>404 Not Found</h1></center>\n<hr><center>%s</center>\n</body></html> ", version.Identifier())

	handler := &Handler{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	handler.respondError(c, http.StatusNotFound, errors.New("missing"))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != expectedBody {
		t.Fatalf("unexpected body: %q", body)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("unexpected content type: %q", got)
	}
}

func TestHandleResizeUnsupportedMediaType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/resize/200x200/foo.bmp", nil)
	c.Params = gin.Params{{Key: "geometry", Value: "200x200"}, {Key: "filepath", Value: "/foo.bmp"}}
	c.Request = req

	version.Override("test-version")
	expectedBody := fmt.Sprintf("<html><head><title>415 Unsupported Media Type</title></head>\n<body>\n<center><h1>415 Unsupported Media Type</h1></center>\n<hr><center>%s</center>\n</body></html> ", version.Identifier())

	handler := &Handler{
		cfg:    &config.Config{Resize: config.ResizeConfig{MaxWidth: 5000, MaxHeight: 5000}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	handler.handleResize(c)

	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != expectedBody {
		t.Fatalf("unexpected body: %q", body)
	}
}
