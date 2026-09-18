package config

import (
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservabilityDefaults(t *testing.T) {
	baseDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	t.Setenv("FARS_STORAGE__BASE_DIR", baseDir)
	t.Setenv("FARS_STORAGE__CACHE_DIR", cacheDir)

	cfg, err := LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if !cfg.Metrics.Enabled {
		t.Error("metrics are off by default; the endpoint should be available unless switched off")
	}
	if cfg.Metrics.Path != "/metrics" {
		t.Errorf("metrics.path = %q, want /metrics", cfg.Metrics.Path)
	}
	if cfg.Metrics.Listen != "" {
		t.Errorf("metrics.listen = %q, want empty (share the API listener)", cfg.Metrics.Listen)
	}
	// Text, not JSON: the deployed promtail pipeline keys FARS lines off
	// `level=INFO`, so flipping this default would silently unlabel the logs.
	if cfg.Logging.Format != "text" {
		t.Errorf("logging.format = %q, want text", cfg.Logging.Format)
	}
	if cfg.Logging.LevelSlog() != slog.LevelInfo {
		t.Errorf("logging level = %v, want info", cfg.Logging.LevelSlog())
	}
	if !cfg.Logging.Access {
		t.Error("the access log should stay on by default")
	}
}

func TestObservabilityEnvOverrides(t *testing.T) {
	baseDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	t.Setenv("FARS_STORAGE__BASE_DIR", baseDir)
	t.Setenv("FARS_STORAGE__CACHE_DIR", cacheDir)
	t.Setenv("FARS_METRICS__ENABLED", "false")
	t.Setenv("FARS_METRICS__PATH", "/internal/metrics")
	t.Setenv("FARS_METRICS__LISTEN", "127.0.0.1:9091")
	t.Setenv("FARS_LOGGING__LEVEL", "warn")
	t.Setenv("FARS_LOGGING__FORMAT", "json")
	t.Setenv("FARS_LOGGING__ACCESS", "false")

	cfg, err := LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if cfg.Metrics.Enabled {
		t.Error("FARS_METRICS__ENABLED=false did not disable metrics")
	}
	if cfg.Metrics.Path != "/internal/metrics" || cfg.Metrics.Listen != "127.0.0.1:9091" {
		t.Errorf("unexpected metrics config: %+v", cfg.Metrics)
	}
	if cfg.Logging.LevelSlog() != slog.LevelWarn {
		t.Errorf("logging level = %v, want warn", cfg.Logging.LevelSlog())
	}
	if !cfg.Logging.JSON() {
		t.Error("FARS_LOGGING__FORMAT=json did not select the JSON handler")
	}
	if cfg.Logging.Access {
		t.Error("FARS_LOGGING__ACCESS=false did not disable the access log")
	}
}

func TestValidateObservability(t *testing.T) {
	base := func(t *testing.T) *Config {
		t.Helper()
		cfg := defaultConfig()
		cfg.Storage.BaseDir = t.TempDir()
		cfg.Storage.CacheDir = filepath.Join(t.TempDir(), "cache")
		return cfg
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "defaults are valid", mutate: func(*Config) {}},
		{
			name:    "path must be absolute",
			mutate:  func(c *Config) { c.Metrics.Path = "metrics" },
			wantErr: "must start with a slash",
		},
		{
			name:    "path must be set",
			mutate:  func(c *Config) { c.Metrics.Path = "  " },
			wantErr: "metrics.path must be set",
		},
		{
			// gin panics on a literal route under an existing wildcard, which
			// would be a crash loop rather than a startup error.
			name:    "path may not collide with the resize routes",
			mutate:  func(c *Config) { c.Metrics.Path = "/resize/metrics" },
			wantErr: "collides with the /resize routes",
		},
		{
			name:    "a disabled endpoint is not validated",
			mutate:  func(c *Config) { c.Metrics.Enabled = false; c.Metrics.Path = "nonsense" },
			wantErr: "",
		},
		{
			name:    "listen must be host:port",
			mutate:  func(c *Config) { c.Metrics.Listen = "9091" },
			wantErr: "must be host:port",
		},
		{
			name:    "listen port must be a port",
			mutate:  func(c *Config) { c.Metrics.Listen = "127.0.0.1:0" },
			wantErr: "port must be between",
		},
		{
			name:    "listen host must be an address",
			mutate:  func(c *Config) { c.Metrics.Listen = "localhost:9091" },
			wantErr: "host must be an IP",
		},
		{
			name: "listen may not duplicate the api listener",
			mutate: func(c *Config) {
				c.Server.Host = "127.0.0.1"
				c.Server.Port = 9090
				c.Metrics.Listen = "127.0.0.1:9090"
			},
			wantErr: "must differ from the server address",
		},
		{
			name:    "level must be known",
			mutate:  func(c *Config) { c.Logging.Level = "verbose" },
			wantErr: "logging.level must be one of",
		},
		{
			name:    "format must be known",
			mutate:  func(c *Config) { c.Logging.Format = "logfmt" },
			wantErr: "logging.format must be text or json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestDefaultsAreTheDeployedValues pins the built-in defaults to what the
// Citimarine storefront runs on, so a deployment that ships no config file
// behaves like the one that does. The encoder numbers in particular were
// arrived at by measurement; a silent drift back to library defaults would
// change every image the service produces.
func TestDefaultsAreTheDeployedValues(t *testing.T) {
	cfg := defaultConfig()

	if cfg.Resize.JPGQuality != 77 || cfg.Resize.WebPQuality != 65 || cfg.Resize.AVIFQuality != 50 {
		t.Errorf("encoder quality defaults = %d/%d/%d, want 77/65/50 (jpg/webp/avif)",
			cfg.Resize.JPGQuality, cfg.Resize.WebPQuality, cfg.Resize.AVIFQuality)
	}
	if cfg.Resize.AVIFSpeed != 6 {
		t.Errorf("avif_speed = %d, want 6: 3 costs several times the CPU per AVIF for a few percent of size", cfg.Resize.AVIFSpeed)
	}
	if cfg.Cache.TTL.Duration != 10*24*time.Hour {
		t.Errorf("ttl = %s, want 240h", cfg.Cache.TTL)
	}
	if cfg.Cache.CleanupInterval.Duration != 12*time.Hour {
		t.Errorf("cleanup_interval = %s, want 12h", cfg.Cache.CleanupInterval)
	}
	// Unbounded by default is how a cache fills a volume: every distinct
	// geometry writes another file and only ttl removes them.
	if cfg.Cache.MaxSize.Bytes != 50<<30 {
		t.Errorf("max_size = %d bytes, want 50gb", cfg.Cache.MaxSize.Bytes)
	}
	// Zero, not a hand-maintained core count: the Go runtime reads the cgroup
	// quota itself and applyRuntimeTuning pins libvips to one thread.
	if cfg.Runtime.GOMAXPROCS != 0 || cfg.Runtime.VIPSConcurrency != 0 || cfg.Runtime.ResizeConcurrency != 0 {
		t.Errorf("runtime defaults = %+v, want all zero (derived at startup)", cfg.Runtime)
	}
	if len(cfg.Server.TrustedProxies) == 0 {
		t.Error("no trusted proxies by default; remote_ip would be the reverse proxy on every line")
	}
	if err := cfg.Validate(); err != nil && !strings.Contains(err.Error(), "storage.") {
		t.Errorf("the defaults do not validate: %v", err)
	}
}

// TestDefaultTrustedProxiesBelieveOnlyPrivatePeers is the safety argument for
// the default above: a client reaching FARS over the internet arrives with a
// public source address, which is not on this list, so it cannot forge its own
// remote_ip.
func TestDefaultTrustedProxiesBelieveOnlyPrivatePeers(t *testing.T) {
	for _, cidr := range defaultConfig().Server.TrustedProxies {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("default trusted proxy %q does not parse: %v", cidr, err)
		}
		if !network.IP.IsPrivate() && !network.IP.IsLoopback() {
			t.Errorf("default trusts %q, which is neither private nor loopback", cidr)
		}
	}
}
