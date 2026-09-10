package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOriginalMonitorDefaults(t *testing.T) {
	cfg := defaultConfig()
	if cfg.Cache.CheckOriginalsInterval.Duration != 5*time.Minute {
		t.Fatalf("unexpected default check interval: %s", cfg.Cache.CheckOriginalsInterval.Duration)
	}
}

func TestOriginalMonitorSettingsFromYAML(t *testing.T) {
	baseDir := t.TempDir()
	cacheDir := t.TempDir()
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
  check_originals_interval: "90s"
  invalidation_token: "test-token"
`, filepath.ToSlash(baseDir), filepath.ToSlash(cacheDir))

	cfg, err := LoadReader(strings.NewReader(yamlConfig))
	if err != nil {
		t.Fatalf("LoadReader: %v", err)
	}
	if cfg.Cache.CheckOriginalsInterval.Duration != 90*time.Second {
		t.Fatalf("unexpected check interval: %s", cfg.Cache.CheckOriginalsInterval.Duration)
	}
	if cfg.Cache.InvalidationToken != "test-token" {
		t.Fatalf("unexpected invalidation token: %q", cfg.Cache.InvalidationToken)
	}
}

func TestOriginalMonitorLegacyEnvironmentOverrides(t *testing.T) {
	t.Setenv("IMAGES_BASE_DIR", t.TempDir())
	t.Setenv("CACHE_DIR", t.TempDir())
	t.Setenv("CHECK_ORIGINALS_INTERVAL", "45s")
	t.Setenv("INVALIDATION_TOKEN", "env-token")

	cfg, err := LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if cfg.Cache.CheckOriginalsInterval.Duration != 45*time.Second {
		t.Fatalf("unexpected check interval: %s", cfg.Cache.CheckOriginalsInterval.Duration)
	}
	if cfg.Cache.InvalidationToken != "env-token" {
		t.Fatalf("unexpected invalidation token: %q", cfg.Cache.InvalidationToken)
	}
}
