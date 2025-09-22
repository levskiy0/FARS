package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fars/pkg/configutil"
)

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"0", 0},
		{"42", 42},
		{"400kb", 400 << 10},
		{"2mb", 2 << 20},
		{"3GB", 3 << 30},
		{"5MiB", 5 << 20},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			size, err := configutil.ParseByteSize(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if size != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, size)
			}
		})
	}
}

func TestParseByteSizeInvalid(t *testing.T) {
	if _, err := configutil.ParseByteSize("12foobar"); err == nil {
		t.Fatalf("expected error for invalid unit")
	}
}

func TestLoadFromEnvOrFileLegacyEnv(t *testing.T) {
	baseDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "cache")

	t.Setenv("HOST", "127.0.0.1")
	t.Setenv("PORT", "9091")
	t.Setenv("IMAGES_BASE_DIR", baseDir)
	t.Setenv("CACHE_DIR", cacheDir)
	t.Setenv("MAX_WIDTH", "1500")
	t.Setenv("MAX_HEIGHT", "800")
	t.Setenv("JPG_QUALITY", "90")
	t.Setenv("WEBP_QUALITY", "88")
	t.Setenv("AVIF_QUALITY", "55")
	t.Setenv("PNG_COMPRESSION", "4")
	t.Setenv("AVIF_SPEED", "6")
	t.Setenv("GOMAXPROCS", "6")
	t.Setenv("VIPS_CONCURRENCY", "5")
	t.Setenv("TTL", "24h")
	t.Setenv("CLEANUP_INTERVAL", "10m")
	cfg, err := LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("unexpected host: %s", cfg.Server.Host)
	}
	if cfg.Server.Port != 9091 {
		t.Fatalf("unexpected port: %d", cfg.Server.Port)
	}
	if cfg.Storage.BaseDir != baseDir {
		t.Fatalf("unexpected base dir: %s", cfg.Storage.BaseDir)
	}
	if cfg.Storage.CacheDir != cacheDir {
		t.Fatalf("unexpected cache dir: %s", cfg.Storage.CacheDir)
	}
	if cfg.Resize.MaxWidth != 1500 || cfg.Resize.MaxHeight != 800 {
		t.Fatalf("unexpected resize limits: %+v", cfg.Resize)
	}
	if cfg.Resize.JPGQuality != 90 || cfg.Resize.WebPQuality != 88 || cfg.Resize.AVIFQuality != 55 || cfg.Resize.PNGCompression != 4 {
		t.Fatalf("unexpected resize quality settings: %+v", cfg.Resize)
	}
	if cfg.Resize.AVIFSpeed != 6 {
		t.Fatalf("unexpected avif speed: %d", cfg.Resize.AVIFSpeed)
	}
	if cfg.Cache.TTL.Duration != 24*time.Hour {
		t.Fatalf("unexpected cache TTL: %s", cfg.Cache.TTL)
	}
	if cfg.Cache.CleanupInterval.Duration != 10*time.Minute {
		t.Fatalf("unexpected cleanup interval: %s", cfg.Cache.CleanupInterval)
	}
	if cfg.Runtime.GOMAXPROCS != 6 {
		t.Fatalf("unexpected GOMAXPROCS: %d", cfg.Runtime.GOMAXPROCS)
	}
	if cfg.Runtime.VIPSConcurrency != 5 {
		t.Fatalf("unexpected vips concurrency: %d", cfg.Runtime.VIPSConcurrency)
	}
}

func TestLoadFromEnvOrFileWithPrefixedKeys(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "prefixed-base")
	cacheDir := filepath.Join(t.TempDir(), "prefixed-cache")

	t.Setenv("FARS_SERVER__HOST", "0.0.0.0")
	t.Setenv("FARS_SERVER__PORT", "8085")
	t.Setenv("FARS_STORAGE__BASE_DIR", baseDir)
	t.Setenv("FARS_STORAGE__CACHE_DIR", cacheDir)
	t.Setenv("FARS_CACHE__TTL", "36h")
	t.Setenv("FARS_CACHE__CLEANUP_INTERVAL", "30m")
	t.Setenv("FARS_RESIZE__MAX_WIDTH", "1800")
	t.Setenv("FARS_RESIZE__MAX_HEIGHT", "900")
	t.Setenv("FARS_RESIZE__AVIF_SPEED", "4")
	t.Setenv("FARS_RUNTIME__GOMAXPROCS", "3")
	t.Setenv("FARS_RUNTIME__VIPS_CONCURRENCY", "7")

	cfg, err := LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if cfg.Server.Port != 8085 {
		t.Fatalf("unexpected port: %d", cfg.Server.Port)
	}
	if cfg.Storage.BaseDir != baseDir || cfg.Storage.CacheDir != cacheDir {
		t.Fatalf("unexpected storage config: %+v", cfg.Storage)
	}
	if cfg.Cache.TTL.Duration != 36*time.Hour {
		t.Fatalf("unexpected ttl: %s", cfg.Cache.TTL)
	}
	if cfg.Cache.CleanupInterval.Duration != 30*time.Minute {
		t.Fatalf("unexpected cleanup interval: %s", cfg.Cache.CleanupInterval)
	}
	if cfg.Resize.MaxWidth != 1800 || cfg.Resize.MaxHeight != 900 {
		t.Fatalf("unexpected resize limits: %+v", cfg.Resize)
	}
	if cfg.Resize.AVIFSpeed != 4 {
		t.Fatalf("unexpected avif speed: %d", cfg.Resize.AVIFSpeed)
	}
	if cfg.Runtime.GOMAXPROCS != 3 || cfg.Runtime.VIPSConcurrency != 7 {
		t.Fatalf("unexpected runtime config: %+v", cfg.Runtime)
	}
}

func TestParseFlexibleDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"0", 0},
		{"30d", 30 * 24 * time.Hour},
		{"1d12h", (24 + 12) * time.Hour},
		{"2h30m", 2*time.Hour + 30*time.Minute},
		{"45m10s", 45*time.Minute + 10*time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			dur, err := configutil.ParseFlexibleDuration(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dur != tt.expected {
				t.Fatalf("expected %s, got %s", tt.expected, dur)
			}
		})
	}
}

func TestResolvePathsWithRewrite(t *testing.T) {
	base := t.TempDir()
	cache := t.TempDir()

	yamlConfig := fmt.Sprintf(`
server:
  host: 127.0.0.1
  port: 9090
storage:
  base_dir: %q
  cache_dir: %q
resize:
  max_width: 2000
  max_height: 2000
  jpg_quality: 80
  webp_quality: 75
  avif_quality: 45
  png_compression: 6
cache:
  ttl: "30d"
  cleanup_interval: "24h"
rewrites:
  - pattern: "^foo/(.+)$"
    replacement: "img/$1"
`, filepath.ToSlash(base), filepath.ToSlash(cache))

	cfg, err := LoadReader(strings.NewReader(yamlConfig))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	rel, full, err := cfg.ResolvePaths("foo/bar/baz.jpg")
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if rel != "img/bar/baz.jpg" {
		t.Fatalf("unexpected relative path: %s", rel)
	}
	expectedFull := filepath.Join(base, "img", "bar", "baz.jpg")
	if full != expectedFull {
		t.Fatalf("unexpected full path: %s", full)
	}

	cachePath := cfg.CachePath(200, 100, rel)
	expectedCache := filepath.Join(cache, "200x100", "img", "bar", "baz.jpg")
	if cachePath != expectedCache {
		t.Fatalf("unexpected cache path: %s", cachePath)
	}
}

func TestCachePathFormatsSingleDimension(t *testing.T) {
	cache := t.TempDir()
	cfg := &Config{Storage: StorageConfig{CacheDir: cache}}

	testCases := []struct {
		name     string
		width    int
		height   int
		relative string
		expected string
	}{
		{
			name:     "width only",
			width:    100,
			relative: "foo/bar.jpg",
			expected: filepath.Join(cache, "100x", "foo", "bar.jpg"),
		},
		{
			name:     "height only",
			height:   150,
			relative: "/foo/bar.jpg",
			expected: filepath.Join(cache, "x150", "foo", "bar.jpg"),
		},
		{
			name:     "zero dimensions",
			relative: "foo/bar.jpg",
			expected: filepath.Join(cache, "0x0", "foo", "bar.jpg"),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cachePath := cfg.CachePath(tc.width, tc.height, tc.relative)
			if cachePath != tc.expected {
				t.Fatalf("unexpected cache path: %s", cachePath)
			}
		})
	}
}

func TestCacheSettingsFromYAML(t *testing.T) {
	base := t.TempDir()
	cache := t.TempDir()
	yamlConfig := fmt.Sprintf(`
server:
  host: 127.0.0.1
  port: 9090
storage:
  base_dir: %q
  cache_dir: %q
resize:
  max_width: 1000
  max_height: 1000
  jpg_quality: 80
  webp_quality: 75
  avif_quality: 45
  png_compression: 6
cache:
  ttl: "30d"
  cleanup_interval: "24h"
`, filepath.ToSlash(base), filepath.ToSlash(cache))

	cfg, err := LoadReader(strings.NewReader(yamlConfig))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Cache.TTL.Duration != 30*24*time.Hour {
		t.Fatalf("unexpected ttl: %s", cfg.Cache.TTL)
	}
	if cfg.Cache.CleanupInterval.Duration != 24*time.Hour {
		t.Fatalf("unexpected cleanup interval: %s", cfg.Cache.CleanupInterval)
	}
}
