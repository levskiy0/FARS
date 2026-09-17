//go:build integration

package httpapi

import (
	"bytes"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"fars/internal/config"
)

// TestHandleResizeCacheMissThenHit renders once (miss), then requests the
// same URL again and checks the second response is a genuine cache hit: same
// bytes, same ETag, and — the proof it wasn't re-encoded — the cache file's
// mtime is unchanged by the second request.
func TestHandleResizeCacheMissThenHit(t *testing.T) {
	cfg, _, _, router := newResizeTestServer(t, config.ResizeConfig{})
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png", pngOfSize(t, 16, 16, color.NRGBA{R: 200, G: 20, B: 20, A: 255}))

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/resize/8x8/img/photo.png", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first request: status=%d body=%s", first.Code, first.Body.String())
	}

	cachePath := filepath.Join(cfg.Storage.CacheDir, "8x8", "img", "photo.png")
	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("expected cache file to exist after miss: %v", err)
	}
	writtenAt := info.ModTime()

	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/resize/8x8/img/photo.png", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second request: status=%d body=%s", second.Code, second.Body.String())
	}

	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("cache hit returned different bytes than the original render")
	}
	if first.Header().Get("ETag") != second.Header().Get("ETag") {
		t.Fatalf("cache hit ETag %q differs from original %q", second.Header().Get("ETag"), first.Header().Get("ETag"))
	}

	infoAfter, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("stat cache file after second request: %v", err)
	}
	if !infoAfter.ModTime().Equal(writtenAt) {
		t.Fatalf("cache file was rewritten on a hit (mtime %v -> %v): request was re-encoded instead of served from cache", writtenAt, infoAfter.ModTime())
	}
}

// TestHandleResizeConditionalRequests covers If-None-Match and
// If-Modified-Since yielding 304 on a freshly rendered response, and locks in
// the fix where a cache hit and a fresh render used different mtimes for
// Last-Modified — which meant IMS revalidation against a cache hit never
// produced a 304.
func TestHandleResizeConditionalRequests(t *testing.T) {
	cfg, _, _, router := newResizeTestServer(t, config.ResizeConfig{})
	originalPath := writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png", pngOfSize(t, 16, 16, color.NRGBA{R: 10, G: 220, B: 10, A: 255}))
	// Backdate the original well before "now" (when the cache file would be
	// written) so a cache-hit branch that used the cache file's own mtime
	// instead of the original's would produce a visibly different
	// Last-Modified, not one that coincidentally rounds to the same second.
	backdated := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(originalPath, backdated, backdated); err != nil {
		t.Fatalf("backdate original: %v", err)
	}

	fresh := httptest.NewRecorder()
	router.ServeHTTP(fresh, httptest.NewRequest(http.MethodGet, "/resize/8x8/img/photo.png", nil))
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh render: status=%d body=%s", fresh.Code, fresh.Body.String())
	}
	etag := fresh.Header().Get("ETag")
	lastModified := fresh.Header().Get("Last-Modified")
	if etag == "" || lastModified == "" {
		t.Fatalf("expected ETag and Last-Modified on fresh render, got %q / %q", etag, lastModified)
	}

	t.Run("If-None-Match on fresh render", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/resize/8x8/img/photo.png", nil)
		req.Header.Set("If-None-Match", etag)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusNotModified {
			t.Fatalf("status=%d, want 304, body=%s", resp.Code, resp.Body.String())
		}
		if resp.Body.Len() != 0 {
			t.Fatalf("expected empty 304 body, got %d bytes", resp.Body.Len())
		}
	})

	t.Run("If-Modified-Since on fresh render", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/resize/8x8/img/photo.png", nil)
		req.Header.Set("If-Modified-Since", time.Now().Add(time.Hour).Format(http.TimeFormat))
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusNotModified {
			t.Fatalf("status=%d, want 304, body=%s", resp.Code, resp.Body.String())
		}
	})

	t.Run("Last-Modified identical between fresh render and cache hit", func(t *testing.T) {
		hit := httptest.NewRecorder()
		router.ServeHTTP(hit, httptest.NewRequest(http.MethodGet, "/resize/8x8/img/photo.png", nil))
		if hit.Code != http.StatusOK {
			t.Fatalf("cache hit: status=%d body=%s", hit.Code, hit.Body.String())
		}
		if hit.Header().Get("Last-Modified") != lastModified {
			t.Fatalf("Last-Modified differs between fresh render (%q) and cache hit (%q)", lastModified, hit.Header().Get("Last-Modified"))
		}

		// With the real Last-Modified from the fresh render, IMS against the
		// now-cached response must also revalidate to 304 — this is exactly
		// the branch that broke when the two paths used different mtimes.
		//
		// http.TimeFormat only has second resolution, so the exact
		// Last-Modified string parses back one whole second below the
		// original's real (sub-second) mtime; a client that echoes it back
		// unmodified needs a one-second buffer here, same as any real
		// browser revalidating on the next tick. What this still catches:
		// under the old bug the cache-hit branch stamped a different (cache
		// file's own) mtime, which could land arbitrarily far from the fresh
		// render's, well outside a one-second buffer.
		parsedLastModified, err := http.ParseTime(lastModified)
		if err != nil {
			t.Fatalf("parse Last-Modified %q: %v", lastModified, err)
		}
		req := httptest.NewRequest(http.MethodGet, "/resize/8x8/img/photo.png", nil)
		req.Header.Set("If-Modified-Since", parsedLastModified.Add(time.Second).Format(http.TimeFormat))
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusNotModified {
			t.Fatalf("IMS against cache hit: status=%d, want 304, body=%s", resp.Code, resp.Body.String())
		}
	})
}

// TestHandleResizeHeadMatchesGet checks HEAD /resize/... returns 200 with a
// Content-Length matching the GET body length and no body of its own. This
// needs the real net/http server (not a bare ResponseRecorder) since HEAD
// body-stripping is enforced by net/http itself, not by the handler.
func TestHandleResizeHeadMatchesGet(t *testing.T) {
	cfg, _, _, router := newResizeTestServer(t, config.ResizeConfig{})
	writeHTTPTestFile(t, cfg.Storage.BaseDir, "img/photo.png", pngOfSize(t, 16, 16, color.NRGBA{R: 5, G: 5, B: 220, A: 255}))

	server := httptest.NewServer(router)
	defer server.Close()

	getResp, err := http.Get(server.URL + "/resize/8x8/img/photo.png")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	getBody, err := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if err != nil {
		t.Fatalf("read GET body: %v", err)
	}
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d", getResp.StatusCode)
	}

	headResp, err := http.Head(server.URL + "/resize/8x8/img/photo.png")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	headBody, err := io.ReadAll(headResp.Body)
	headResp.Body.Close()
	if err != nil {
		t.Fatalf("read HEAD body: %v", err)
	}
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status=%d", headResp.StatusCode)
	}
	if len(headBody) != 0 {
		t.Fatalf("expected empty HEAD body, got %d bytes", len(headBody))
	}
	wantLength := len(getBody)
	if got := headResp.Header.Get("Content-Length"); got != strconv.Itoa(wantLength) {
		t.Fatalf("HEAD Content-Length=%q, want %d (GET body length)", got, wantLength)
	}
}
