package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/httpapi"
	"fars/internal/processor"
)

func newMetricsTestEngine(t *testing.T, metricsCfg config.MetricsConfig) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Server:  config.ServerConfig{Host: "127.0.0.1", Port: 8080},
		Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()},
		Resize:  config.ResizeConfig{MaxWidth: 100, MaxHeight: 100},
		Metrics: metricsCfg,
	}
	handler := httpapi.NewHandler(cfg, cache.NewManager(cfg, logger), processor.New(), logger)
	return NewEngine(cfg, handler)
}

func TestMetricsEndpointExposesFarsCollectors(t *testing.T) {
	engine := newMetricsTestEngine(t, config.MetricsConfig{Enabled: true, Path: "/metrics"})

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// One name per family that an operator would actually build a dashboard
	// or an alert on: if any of these disappears, the deployment's queries
	// break silently.
	for _, want := range []string{
		"fars_build_info",
		"fars_resize_duration_seconds",
		"fars_resize_queue_wait_seconds",
		"fars_resize_in_flight",
		"fars_resize_slots",
		"fars_cache_removals_total",
		"fars_cache_size_bytes",
		"fars_cache_sweep_last_success_timestamp_seconds",
		"fars_originals_index_ready",
		"fars_originals_tracked",
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition is missing %q", want)
		}
	}
}

// The request families carry a status code, so they are not pre-created; they
// have to show up once a request has actually been served.
func TestMetricsEndpointCountsServedRequests(t *testing.T) {
	engine := newMetricsTestEngine(t, config.MetricsConfig{Enabled: true, Path: "/metrics"})

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/resize/10x10/missing.jpg", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("resize status = %d, want 404 for a missing original", rec.Code)
	}

	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	want := `fars_http_requests_total{cache="none",code="404",method="GET",route="resize"}`
	if !strings.Contains(body, want) {
		t.Errorf("exposition does not contain %s", want)
	}
	// Collectors are process-global, so another test in this package may have
	// already moved these. The assertion is "it counted", not "it is exactly 1".
	if v := sampleValue(t, body, `fars_request_errors_total{reason="not_found"}`); v < 1 {
		t.Errorf("not_found errors = %v, want at least 1", v)
	}
	if v := sampleValue(t, body, `fars_http_request_duration_seconds_count{cache="none",route="resize"}`); v < 1 {
		t.Errorf("resize duration observations = %v, want at least 1", v)
	}
}

// sampleValue returns the value of the exposition line starting with series,
// or 0 when the series is absent.
func sampleValue(t *testing.T, body, series string) float64 {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		rest, ok := strings.CutPrefix(line, series+" ")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		return value
	}
	return 0
}

func TestMetricsEndpointIsNotCountedAsTraffic(t *testing.T) {
	engine := newMetricsTestEngine(t, config.MetricsConfig{Enabled: true, Path: "/metrics"})

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if strings.Contains(rec.Body.String(), `route="unknown"`) {
			t.Fatal("the scrape counted itself as traffic")
		}
	}
}

func TestMetricsEndpointDisabled(t *testing.T) {
	engine := newMetricsTestEngine(t, config.MetricsConfig{Enabled: false, Path: "/metrics"})

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when metrics.enabled is false", rec.Code)
	}
}

func TestMetricsEndpointMovesToItsOwnListener(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{Host: "127.0.0.1", Port: 8080},
		Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()},
		Resize:  config.ResizeConfig{MaxWidth: 100, MaxHeight: 100},
		Metrics: config.MetricsConfig{Enabled: true, Path: "/metrics", Listen: "127.0.0.1:9091"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := httpapi.NewHandler(cfg, cache.NewManager(cfg, logger), processor.New(), logger)

	engine := NewEngine(cfg, handler)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("main listener status = %d, want 404 once metrics moved to their own port", rec.Code)
	}

	srv := newMetricsServer(cfg)
	if srv == nil {
		t.Fatal("newMetricsServer returned nil for a configured listen address")
	}
	if srv.Addr != "127.0.0.1:9091" {
		t.Fatalf("metrics addr = %q, want 127.0.0.1:9091", srv.Addr)
	}
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dedicated listener status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "fars_build_info") {
		t.Error("dedicated listener did not serve the FARS collectors")
	}
}

func TestMetricsServerNilWhenShared(t *testing.T) {
	cfg := &config.Config{Metrics: config.MetricsConfig{Enabled: true, Path: "/metrics"}}
	if srv := newMetricsServer(cfg); srv != nil {
		t.Fatalf("newMetricsServer = %v, want nil when the endpoint shares the API listener", srv.Addr)
	}
	cfg.Metrics.Enabled = false
	cfg.Metrics.Listen = "127.0.0.1:9091"
	if srv := newMetricsServer(cfg); srv != nil {
		t.Fatal("newMetricsServer returned a server while metrics are disabled")
	}
}

// TestPanicIsCountedAsAServedFiveHundred pins the middleware order. With
// ObserveRequests registered inside gin.Recovery(), a panic unwound straight
// past its bookkeeping: the client got a 500 that appeared in no counter, no
// duration histogram and no error total — invisible exactly where visibility
// matters most.
func TestPanicIsCountedAsAServedFiveHundred(t *testing.T) {
	engine := newMetricsTestEngine(t, config.MetricsConfig{Enabled: true, Path: "/metrics"})
	engine.GET("/boom", func(c *gin.Context) { panic("libvips said no") })

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if v := sampleValue(t, body, `fars_http_requests_total{cache="none",code="500",method="GET",route="unknown"}`); v < 1 {
		t.Errorf("panicking request counted %v times, want at least 1", v)
	}
	if v := sampleValue(t, body, `fars_http_request_duration_seconds_count{cache="none",route="unknown"}`); v < 1 {
		t.Errorf("panicking request produced %v duration samples, want at least 1", v)
	}
}
