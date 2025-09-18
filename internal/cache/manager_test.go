package cache

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"log/slog"

	"fars/internal/config"
)

func TestCleanupPreservesDerivedCacheWhenOriginalExists(t *testing.T) {
	baseDir := t.TempDir()
	cacheDir := t.TempDir()

	originalPath := filepath.Join(baseDir, "img", "photo.jpg")
	if err := os.MkdirAll(filepath.Dir(originalPath), 0o755); err != nil {
		t.Fatalf("mkdir original dir: %v", err)
	}
	if err := os.WriteFile(originalPath, []byte("original"), 0o644); err != nil {
		t.Fatalf("write original: %v", err)
	}

	cachePath := filepath.Join(cacheDir, "200x200", "img", "photo.jpg.webp")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatalf("mkdir cache dir: %v", err)
	}
	if err := os.WriteFile(cachePath, []byte("cached"), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	// Ensure cache file appears newer than original so it wouldn't be evicted as stale.
	recent := time.Now()
	if err := os.Chtimes(cachePath, recent, recent); err != nil {
		t.Fatalf("bump cache mtime: %v", err)
	}

	cfg := &config.Config{
		Storage: config.StorageConfig{
			BaseDir:  baseDir,
			CacheDir: cacheDir,
		},
		Cache: config.CacheConfig{
			TTL: config.Duration{Duration: 30 * 24 * time.Hour},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := NewManager(cfg, logger)

	if err := manager.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}

	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("expected cache file to remain, got error: %v", err)
	}
}
