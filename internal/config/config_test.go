package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
