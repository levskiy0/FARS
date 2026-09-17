package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setStorageEnv points the two storage directories at real temporary paths so
// every case below exercises the override under test and nothing else.
func setStorageEnv(t *testing.T) (baseDir, cacheDir string) {
	t.Helper()
	baseDir = t.TempDir()
	cacheDir = filepath.Join(t.TempDir(), "cache")
	t.Setenv("IMAGES_BASE_DIR", baseDir)
	t.Setenv("CACHE_DIR", cacheDir)
	return baseDir, cacheDir
}

func TestPrefixedEnvWinsOverLegacyName(t *testing.T) {
	setStorageEnv(t)
	t.Setenv("PORT", "9091")
	t.Setenv("FARS_PORT", "9092")
	cfg, err := LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if cfg.Server.Port != 9092 {
		t.Fatalf("legacy PORT beat FARS_PORT: got %d", cfg.Server.Port)
	}
}

// TestLegacyEnvAllowlistStillWorks pins the names the shipped Dockerfile sets.
func TestLegacyEnvAllowlistStillWorks(t *testing.T) {
	baseDir, cacheDir := setStorageEnv(t)
	t.Setenv("PORT", "9093")
	t.Setenv("TTL", "7d")
	t.Setenv("CLEANUP_INTERVAL", "6h")
	cfg, err := LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if cfg.Server.Port != 9093 {
		t.Fatalf("PORT ignored: %d", cfg.Server.Port)
	}
	if cfg.Storage.BaseDir != baseDir {
		t.Fatalf("IMAGES_BASE_DIR ignored: %s", cfg.Storage.BaseDir)
	}
	if cfg.Storage.CacheDir != cacheDir {
		t.Fatalf("CACHE_DIR ignored: %s", cfg.Storage.CacheDir)
	}
	if cfg.Cache.TTL.Duration != 7*24*time.Hour {
		t.Fatalf("TTL ignored: %s", cfg.Cache.TTL.Duration)
	}
	if cfg.Cache.CleanupInterval.Duration != 6*time.Hour {
		t.Fatalf("CLEANUP_INTERVAL ignored: %s", cfg.Cache.CleanupInterval.Duration)
	}
}

func TestUnprefixedHostIsIgnoredButPrefixedHostApplies(t *testing.T) {
	setStorageEnv(t)
	t.Setenv("HOST", "10.9.9.9")
	cfg, err := LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if cfg.Server.Host == "10.9.9.9" {
		t.Fatal("ambient HOST repointed the listen address")
	}
	if cfg.Server.Host != defaultConfig().Server.Host {
		t.Fatalf("unexpected host: %q", cfg.Server.Host)
	}
	t.Setenv("FARS_HOST", "127.0.0.9")
	cfg, err = LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if cfg.Server.Host != "127.0.0.9" {
		t.Fatalf("FARS_HOST ignored: %q", cfg.Server.Host)
	}
}

func TestNestedEnvSyntaxRequiresThePrefix(t *testing.T) {
	setStorageEnv(t)
	t.Setenv("SERVER__PORT", "9099")
	t.Setenv("RESIZE__MAX_WIDTH", "111")
	cfg, err := LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	defaults := defaultConfig()
	if cfg.Server.Port != defaults.Server.Port {
		t.Fatalf("unprefixed nested SERVER__PORT was honoured: %d", cfg.Server.Port)
	}
	if cfg.Resize.MaxWidth != defaults.Resize.MaxWidth {
		t.Fatalf("unprefixed nested RESIZE__MAX_WIDTH was honoured: %d", cfg.Resize.MaxWidth)
	}
	t.Setenv("FARS_SERVER__PORT", "9099")
	t.Setenv("FARS_RESIZE__MAX_WIDTH", "111")
	cfg, err = LoadFromEnvOrFile("")
	if err != nil {
		t.Fatalf("LoadFromEnvOrFile: %v", err)
	}
	if cfg.Server.Port != 9099 || cfg.Resize.MaxWidth != 111 {
		t.Fatalf("prefixed nested syntax ignored: port=%d max_width=%d", cfg.Server.Port, cfg.Resize.MaxWidth)
	}
}

func TestMaxCacheSizeFromEnv(t *testing.T) {
	for _, name := range []string{"MAX_CACHE_SIZE", "FARS_MAX_CACHE_SIZE"} {
		t.Run(name, func(t *testing.T) {
			setStorageEnv(t)
			t.Setenv(name, "50gb")
			cfg, err := LoadFromEnvOrFile("")
			if err != nil {
				t.Fatalf("LoadFromEnvOrFile: %v", err)
			}
			if cfg.Cache.MaxSize.Bytes != 50<<30 {
				t.Fatalf("%s not parsed as a human size: %d", name, cfg.Cache.MaxSize.Bytes)
			}
		})
	}
}

func TestDurationAcceptsBareSecondsAndStringForms(t *testing.T) {
	cases := []struct {
		name            string
		ttl             string
		cleanup         string
		wantTTL         time.Duration
		wantCleanupTime time.Duration
	}{
		{"bare integer seconds", "3600", "60", time.Hour, time.Minute},
		{"string forms", `"30d"`, `"24h"`, 30 * 24 * time.Hour, 24 * time.Hour},
		{"zero", "0", "0", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`
storage:
  base_dir: %q
  cache_dir: %q
cache:
  ttl: %s
  cleanup_interval: %s
`, filepath.ToSlash(t.TempDir()), filepath.ToSlash(t.TempDir()), tc.ttl, tc.cleanup)
			cfg, err := LoadReader(strings.NewReader(raw))
			if err != nil {
				t.Fatalf("LoadReader: %v", err)
			}
			if cfg.Cache.TTL.Duration != tc.wantTTL {
				t.Fatalf("ttl %s decoded as %s, want %s", tc.ttl, cfg.Cache.TTL.Duration, tc.wantTTL)
			}
			if cfg.Cache.CleanupInterval.Duration != tc.wantCleanupTime {
				t.Fatalf("cleanup_interval %s decoded as %s, want %s", tc.cleanup, cfg.Cache.CleanupInterval.Duration, tc.wantCleanupTime)
			}
		})
	}
}

func TestMaxSizeFromYAMLHumanString(t *testing.T) {
	raw := fmt.Sprintf(`
storage:
  base_dir: %q
  cache_dir: %q
cache:
  max_size: "300mb"
`, filepath.ToSlash(t.TempDir()), filepath.ToSlash(t.TempDir()))
	cfg, err := LoadReader(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("LoadReader: %v", err)
	}
	if cfg.Cache.MaxSize.Bytes != 300<<20 {
		t.Fatalf("unexpected max_size: %d", cfg.Cache.MaxSize.Bytes)
	}
}
