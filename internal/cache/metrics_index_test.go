package cache

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"fars/internal/config"
	"fars/internal/metrics"
)

// gaugeValue reads one unlabelled gauge out of the FARS registry.
func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			return m.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %q is not exposed", name)
	return 0
}

// TestOriginalsGaugesFollowTheIndex pins the reason these two are collected at
// scrape time. They used to be set only where discovery finished, so a variant
// published by a request left fars_originals_tracked reading whatever the last
// discovery pass had seen — 0 on a cold cache, however much was cached since.
//
// The callback behind them is process-global (one manager per process), so
// this test relies on being the most recent NewManager in the package.
func TestOriginalsGaugesFollowTheIndex(t *testing.T) {
	baseDir := t.TempDir()
	cacheDir := t.TempDir()
	original := filepath.Join(baseDir, "photo.jpg")
	if err := os.WriteFile(original, []byte("not really a jpeg, but a real file"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Storage: config.StorageConfig{BaseDir: baseDir, CacheDir: cacheDir}}
	manager := NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := gaugeValue(t, "fars_originals_tracked"); got != 0 {
		t.Fatalf("tracked originals before any write = %v, want 0", got)
	}

	info, err := os.Stat(original)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := cfg.CachePath(100, 100, "photo.jpg")
	if err := manager.Write(cachePath, "photo.jpg", info, []byte("variant bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := gaugeValue(t, "fars_originals_tracked"); got != 1 {
		t.Fatalf("tracked originals after publishing a variant = %v, want 1", got)
	}
	if _, err := manager.InvalidateOriginals(t.Context(), []string{"photo.jpg"}); err != nil {
		t.Fatalf("InvalidateOriginals: %v", err)
	}
	if got := gaugeValue(t, "fars_originals_tracked"); got != 0 {
		t.Fatalf("tracked originals after invalidation = %v, want 0", got)
	}
}
