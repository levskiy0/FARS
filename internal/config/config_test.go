package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			size, err := parseByteSize(tc.input)
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
	if _, err := parseByteSize("12foobar"); err == nil {
		t.Fatalf("expected error for invalid unit")
	}
}

func TestParseFlexibleDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"1d12h", (24 + 12) * time.Hour},
		{"2h30m", 2*time.Hour + 30*time.Minute},
		{"45m10s", 45*time.Minute + 10*time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			dur, err := parseFlexibleDuration(tt.input)
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

func TestCacheMemorySettingsFromYAML(t *testing.T) {
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
  memory_cache_size: "300mb"
  max_memory_chunk: "400kb"
  storage_hot_cache_size: "100mb"
`, filepath.ToSlash(base), filepath.ToSlash(cache))

	cfg, err := LoadReader(strings.NewReader(yamlConfig))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := cfg.Cache.MemoryCacheSize.Bytes; got != 300<<20 {
		t.Fatalf("unexpected memory cache size: %d", got)
	}
	if got := cfg.Cache.MaxMemoryChunk.Bytes; got != 400<<10 {
		t.Fatalf("unexpected max memory chunk: %d", got)
	}
	if got := cfg.Cache.StorageHotCacheSize.Bytes; got != 100<<20 {
		t.Fatalf("unexpected storage hot cache size: %d", got)
	}
}
