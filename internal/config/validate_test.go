package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validConfig returns a configuration that Validate accepts, with storage
// directories that really exist, so each case below isolates a single rule.
func validConfig(t *testing.T) *Config {
	t.Helper()
	cfg := defaultConfig()
	cfg.Storage.BaseDir = t.TempDir()
	cfg.Storage.CacheDir = filepath.Join(t.TempDir(), "cache")
	return cfg
}

func TestValidateAcceptsBaselineConfig(t *testing.T) {
	if err := validConfig(t).Validate(); err != nil {
		t.Fatalf("baseline configuration rejected: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"empty server.host", func(c *Config) { c.Server.Host = "" }, "server.host"},
		{"blank server.host", func(c *Config) { c.Server.Host = "   " }, "server.host"},
		{"zero server.port", func(c *Config) { c.Server.Port = 0 }, "server.port"},
		{"negative server.port", func(c *Config) { c.Server.Port = -1 }, "server.port"},
		{"server.port above range", func(c *Config) { c.Server.Port = 65536 }, "server.port"},
		{"empty storage.base_dir", func(c *Config) { c.Storage.BaseDir = "" }, "storage.base_dir"},
		{"empty storage.cache_dir", func(c *Config) { c.Storage.CacheDir = "" }, "storage.cache_dir"},
		{"zero resize.max_width", func(c *Config) { c.Resize.MaxWidth = 0 }, "max dimensions"},
		{"negative resize.max_width", func(c *Config) { c.Resize.MaxWidth = -1 }, "max dimensions"},
		{"zero resize.max_height", func(c *Config) { c.Resize.MaxHeight = 0 }, "max dimensions"},
		{"negative resize.max_height", func(c *Config) { c.Resize.MaxHeight = -1 }, "max dimensions"},
		{"zero resize.jpg_quality", func(c *Config) { c.Resize.JPGQuality = 0 }, "resize.jpg_quality"},
		{"resize.jpg_quality above range", func(c *Config) { c.Resize.JPGQuality = 101 }, "resize.jpg_quality"},
		{"negative resize.webp_quality", func(c *Config) { c.Resize.WebPQuality = -1 }, "resize.webp_quality"},
		{"resize.webp_quality above range", func(c *Config) { c.Resize.WebPQuality = 101 }, "resize.webp_quality"},
		{"negative resize.avif_quality", func(c *Config) { c.Resize.AVIFQuality = -1 }, "resize.avif_quality"},
		{"resize.avif_quality above range", func(c *Config) { c.Resize.AVIFQuality = 101 }, "resize.avif_quality"},
		{"negative resize.png_compression", func(c *Config) { c.Resize.PNGCompression = -1 }, "resize.png_compression"},
		{"resize.png_compression above range", func(c *Config) { c.Resize.PNGCompression = 10 }, "resize.png_compression"},
		{"negative resize.avif_speed", func(c *Config) { c.Resize.AVIFSpeed = -1 }, "resize.avif_speed"},
		{"resize.avif_speed above range", func(c *Config) { c.Resize.AVIFSpeed = 9 }, "resize.avif_speed"},
		{"negative runtime.gomaxprocs", func(c *Config) { c.Runtime.GOMAXPROCS = -1 }, "runtime.gomaxprocs"},
		{"negative runtime.vips_concurrency", func(c *Config) { c.Runtime.VIPSConcurrency = -1 }, "runtime.vips_concurrency"},
		{"negative runtime.resize_concurrency", func(c *Config) { c.Runtime.ResizeConcurrency = -1 }, "runtime.resize_concurrency"},
		{"empty server.trusted_proxies entry", func(c *Config) {
			c.Server.TrustedProxies = []string{"10.0.0.1", "  "}
		}, "server.trusted_proxies[1]"},
		{"malformed server.trusted_proxies CIDR", func(c *Config) {
			c.Server.TrustedProxies = []string{"10.0.0.0/33"}
		}, "server.trusted_proxies[0]"},
		{"malformed server.trusted_proxies address", func(c *Config) {
			c.Server.TrustedProxies = []string{"not-an-ip"}
		}, "server.trusted_proxies[0]"},
		{"negative cache.invalidation_lock_timeout", func(c *Config) {
			c.Cache.InvalidationLockTimeout = Duration{-time.Millisecond}
		}, "cache.invalidation_lock_timeout"},
		{"negative cache.max_size", func(c *Config) { c.Cache.MaxSize = ByteSize{-1} }, "cache.max_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("accepted invalid configuration")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name the offending setting %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateAcceptsBoundaryValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"server.port lowest", func(c *Config) { c.Server.Port = 1 }},
		{"server.port highest", func(c *Config) { c.Server.Port = 65535 }},
		{"resize.max dimensions of one", func(c *Config) { c.Resize.MaxWidth, c.Resize.MaxHeight = 1, 1 }},
		{"resize.jpg_quality lowest", func(c *Config) { c.Resize.JPGQuality = 1 }},
		{"resize.jpg_quality highest", func(c *Config) { c.Resize.JPGQuality = 100 }},
		{"resize.webp_quality lowest", func(c *Config) { c.Resize.WebPQuality = 0 }},
		{"resize.webp_quality highest", func(c *Config) { c.Resize.WebPQuality = 100 }},
		{"resize.avif_quality lowest", func(c *Config) { c.Resize.AVIFQuality = 0 }},
		{"resize.avif_quality highest", func(c *Config) { c.Resize.AVIFQuality = 100 }},
		{"resize.png_compression lowest", func(c *Config) { c.Resize.PNGCompression = 0 }},
		{"resize.png_compression highest", func(c *Config) { c.Resize.PNGCompression = 9 }},
		{"resize.avif_speed lowest", func(c *Config) { c.Resize.AVIFSpeed = 0 }},
		{"resize.avif_speed highest", func(c *Config) { c.Resize.AVIFSpeed = 8 }},
		{"runtime.gomaxprocs unset", func(c *Config) { c.Runtime.GOMAXPROCS = 0 }},
		{"runtime.vips_concurrency unset", func(c *Config) { c.Runtime.VIPSConcurrency = 0 }},
		{"cache.check_originals_workers lowest", func(c *Config) { c.Cache.CheckOriginalsWorkers = 0 }},
		{"cache.check_originals_workers highest", func(c *Config) { c.Cache.CheckOriginalsWorkers = 64 }},
		{"cache.invalidation_lock_timeout zero", func(c *Config) { c.Cache.InvalidationLockTimeout = Duration{} }},
		{"cache.max_size unlimited", func(c *Config) { c.Cache.MaxSize = ByteSize{} }},
		{"cache.max_size set", func(c *Config) { c.Cache.MaxSize = ByteSize{50 << 30} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			tc.mutate(cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("rejected a valid value: %v", err)
			}
		})
	}
}

func TestValidateRejectsUnknownErrorIsGeometrySentinel(t *testing.T) {
	cfg := validConfig(t)
	cfg.Resize.MaxWidth = 0
	if err := cfg.Validate(); !errors.Is(err, errInvalidGeometryLimit) {
		t.Fatalf("expected the geometry sentinel, got %v", err)
	}
}

func TestLoadRejectsBadRewriteRules(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		wantErr string
	}{
		{"empty pattern", `""`, "empty pattern"},
		{"blank pattern", `"   "`, "empty pattern"},
		{"uncompilable regex", `"^foo([0-9]+$"`, "compile rewrite rule"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`
storage:
  base_dir: %q
  cache_dir: %q
rewrites:
  - pattern: %s
    replacement: "img/$1"
`, filepath.ToSlash(t.TempDir()), filepath.ToSlash(t.TempDir()), tc.pattern)
			_, err := LoadReader(strings.NewReader(raw))
			if err == nil {
				t.Fatal("accepted an invalid rewrite rule")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not explain the rewrite failure (want %q)", err, tc.wantErr)
			}
		})
	}
}

func TestLoadAcceptsValidRewriteRule(t *testing.T) {
	raw := fmt.Sprintf(`
storage:
  base_dir: %q
  cache_dir: %q
rewrites:
  - pattern: "^foo/(.+)$"
    replacement: "img/$1"
`, filepath.ToSlash(t.TempDir()), filepath.ToSlash(t.TempDir()))
	cfg, err := LoadReader(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("rejected a valid rewrite rule: %v", err)
	}
	if got := cfg.ApplyRewrites("foo/bar.jpg"); got != "img/bar.jpg" {
		t.Fatalf("rewrite not compiled: %q", got)
	}
}

// TestValidateAcceptsTrustedProxies keeps the negative cases above honest:
// the forms gin actually takes must pass.
func TestValidateAcceptsTrustedProxies(t *testing.T) {
	cfg := validConfig(t)
	cfg.Server.TrustedProxies = []string{"10.0.0.1", "172.16.0.0/12", "::1", "fd00::/8"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid trusted proxy list rejected: %v", err)
	}
}
