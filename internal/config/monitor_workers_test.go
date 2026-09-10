package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMonitorWorkerSettings(t *testing.T) {
	raw := fmt.Sprintf("storage:\n  base_dir: %q\n  cache_dir: %q\ncache:\n  check_originals_workers: 8\n  invalidation_lock_timeout: 250ms\n", t.TempDir(), t.TempDir())
	cfg, err := LoadReader(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.CheckOriginalsWorkers != 8 || cfg.Cache.InvalidationLockTimeout.Duration != 250*time.Millisecond {
		t.Fatalf("bad monitor config: %+v", cfg.Cache)
	}
	t.Setenv("FARS_CACHE__CHECK_ORIGINALS_WORKERS", "2")
	t.Setenv("FARS_CACHE__INVALIDATION_LOCK_TIMEOUT", "50ms")
	cfg, err = LoadReader(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.CheckOriginalsWorkers != 2 || cfg.Cache.InvalidationLockTimeout.Duration != 50*time.Millisecond {
		t.Fatalf("bad environment override: %+v", cfg.Cache)
	}
}
func TestInvalidMonitorWorkerSettings(t *testing.T) {
	for _, workers := range []int{-1, 65} {
		cfg := defaultConfig()
		cfg.Cache.CheckOriginalsWorkers = workers
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted %d workers", workers)
		}
	}
	cfg := defaultConfig()
	cfg.Cache.InvalidationLockTimeout.Duration = -time.Millisecond
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted negative lock timeout")
	}
}
