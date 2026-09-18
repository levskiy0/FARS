package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"fars/internal/config"
	"fars/internal/metrics"
)

func TestErrorReasonIsBounded(t *testing.T) {
	// The label may never carry the error text: it embeds the requested path
	// and geometry, which would mint a series per URL.
	for code, want := range map[int]string{
		http.StatusBadRequest:           "bad_request",
		http.StatusUnauthorized:         "unauthorized",
		http.StatusNotFound:             "not_found",
		http.StatusUnsupportedMediaType: "unsupported_media",
		http.StatusInternalServerError:  "internal",
		http.StatusTeapot:               "internal",
	} {
		if got := errorReason(code); got != want {
			t.Errorf("errorReason(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestObserveRequestsCollapsesUnroutedPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(ObserveRequests("/metrics"))

	before := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues("unknown", http.MethodGet, "404", metrics.CacheNone))
	for _, path := range []string{"/nope", "/also-nope", "/deep/er/still"} {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, rec.Code)
		}
	}
	after := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues("unknown", http.MethodGet, "404", metrics.CacheNone))

	if delta := after - before; delta != 3 {
		t.Fatalf("unknown-route counter moved by %v, want 3 (one series for all three paths)", delta)
	}
}

func TestObserveRequestsSkipsTheScrapePath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(ObserveRequests("/metrics"))
	engine.GET("/metrics", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	before := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues("unknown", http.MethodGet, "200", metrics.CacheNone))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	after := testutil.ToFloat64(metrics.HTTPRequests.WithLabelValues("unknown", http.MethodGet, "200", metrics.CacheNone))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if after != before {
		t.Fatalf("the scrape counted itself: %v -> %v", before, after)
	}
}

func TestAccessLogHonoursTheToggle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name    string
		enabled bool
		wantLog bool
	}{
		{name: "on by default", enabled: true, wantLog: true},
		{name: "off when logging.access is false", enabled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			cfg := &config.Config{Logging: config.LoggingConfig{Access: tc.enabled}}
			h := &Handler{cfg: cfg, logger: slog.New(slog.NewTextHandler(&buf, nil))}

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/resize/10x10/a.jpg", nil)
			h.logAccess(c, 10, 10, "a.jpg", time.Now(), true, time.Millisecond)

			if logged := bytes.Contains(buf.Bytes(), []byte("served image")); logged != tc.wantLog {
				t.Fatalf("access line logged = %v, want %v (output: %q)", logged, tc.wantLog, buf.String())
			}
		})
	}
}
