package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/httpapi"
	"fars/internal/processor"
	"github.com/gin-gonic/gin"
)

// TestNewEngineHonoursTrustedProxies checks the X-Forwarded-For header is
// believed only when it comes from a proxy listed in server.trusted_proxies.
// The engine used to hardcode SetTrustedProxies(nil), which made remote_ip
// permanently the address of the Angie instance in front of FARS.
func TestNewEngineHonoursTrustedProxies(t *testing.T) {
	tests := []struct {
		name     string
		trusted  []string
		wantAddr string
	}{
		{name: "nothing trusted by default", wantAddr: "192.0.2.10"},
		{name: "listed proxy is believed", trusted: []string{"192.0.2.10"}, wantAddr: "203.0.113.9"},
		{name: "cidr form is believed", trusted: []string{"192.0.2.0/24"}, wantAddr: "203.0.113.9"},
		{name: "another proxy is not", trusted: []string{"198.51.100.7"}, wantAddr: "192.0.2.10"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			cfg := &config.Config{
				Server:  config.ServerConfig{Host: "127.0.0.1", Port: 8080, TrustedProxies: tc.trusted},
				Storage: config.StorageConfig{BaseDir: t.TempDir(), CacheDir: t.TempDir()},
				Resize:  config.ResizeConfig{MaxWidth: 100, MaxHeight: 100},
			}
			handler := httpapi.NewHandler(cfg, cache.NewManager(cfg, logger), processor.New(), logger)
			engine := NewEngine(cfg, handler)
			engine.GET("/whoami", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })

			request := httptest.NewRequest(http.MethodGet, "/whoami", nil)
			request.RemoteAddr = "192.0.2.10:44444"
			request.Header.Set("X-Forwarded-For", "203.0.113.9")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			if got := response.Body.String(); got != tc.wantAddr {
				t.Fatalf("client ip = %q, want %q", got, tc.wantAddr)
			}
		})
	}
}
