package cache

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fars/internal/config"
)

func TestOriginalMonitorInvalidatesAllVariantsWithPreservedMtime(t *testing.T) {
	manager, baseDir, cacheDir := newTestManager(t)
	originalRel := "img/photo.jpg"
	originalPath := writeTestFile(t, baseDir, originalRel, []byte("original-a"))
	variantA := writeTestFile(t, cacheDir, "200x200/img/photo.jpg", []byte("resize-a"))
	variantB := writeTestFile(t, cacheDir, "400x400/img/photo.jpg.webp", []byte("resize-b"))

	mtime := time.Unix(1_700_000_000, 0)
	setTestMtime(t, originalPath, mtime)
	setTestMtime(t, variantA, mtime.Add(time.Minute))
	setTestMtime(t, variantB, mtime.Add(time.Minute))

	if err := bootstrapForTest(manager); err != nil {
		t.Fatalf("bootstrapIndex: %v", err)
	}
	if got := manager.Stats(); got != (IndexStats{Originals: 1, Variants: 2}) {
		t.Fatalf("unexpected index after bootstrap: %+v", got)
	}

	replacement := filepath.Join(baseDir, "replacement.jpg")
	if err := os.WriteFile(replacement, []byte("original-b"), 0o644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	setTestMtime(t, replacement, mtime)
	if err := os.Rename(replacement, originalPath); err != nil {
		t.Fatalf("replace original: %v", err)
	}

	if err := manager.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatalf("checkOriginalsOnce: %v", err)
	}
	assertNotExists(t, variantA)
	assertNotExists(t, variantB)
	if got := manager.Stats(); got != (IndexStats{}) {
		t.Fatalf("expected empty index after invalidation, got %+v", got)
	}
}

func TestBootstrapUsesManifestToDetectChangeWhileStopped(t *testing.T) {
	manager, baseDir, cacheDir := newTestManager(t)
	originalRel := "img/photo.jpg"
	originalPath := writeTestFile(t, baseDir, originalRel, []byte("original-a"))
	variant := writeTestFile(t, cacheDir, "200x200/img/photo.jpg", []byte("resize-a"))

	mtime := time.Unix(1_700_000_000, 0)
	setTestMtime(t, originalPath, mtime)
	setTestMtime(t, variant, mtime.Add(time.Minute))
	if err := bootstrapForTest(manager); err != nil {
		t.Fatalf("first bootstrapIndex: %v", err)
	}

	replacement := filepath.Join(baseDir, "replacement.jpg")
	if err := os.WriteFile(replacement, []byte("original-b"), 0o644); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	setTestMtime(t, replacement, mtime)
	if err := os.Rename(replacement, originalPath); err != nil {
		t.Fatalf("replace original: %v", err)
	}

	restarted := newManagerForDirs(baseDir, cacheDir)
	if err := bootstrapForTest(restarted); err != nil {
		t.Fatalf("second bootstrapIndex: %v", err)
	}
	assertNotExists(t, variant)
}

func TestWriteRegistersVariantAndManualInvalidationRemovesIt(t *testing.T) {
	manager, baseDir, cacheDir := newTestManager(t)
	originalRel := "img/photo.jpg"
	originalPath := writeTestFile(t, baseDir, originalRel, []byte("original"))
	originalInfo, err := os.Stat(originalPath)
	if err != nil {
		t.Fatalf("stat original: %v", err)
	}
	variant := filepath.Join(cacheDir, "200x200", filepath.FromSlash(originalRel))

	releaseOriginal := manager.LockOriginal(originalRel)
	releaseCache := manager.LockCache(variant)
	if err := manager.Write(variant, originalRel, originalInfo, []byte("resize")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	releaseCache()
	releaseOriginal()

	if got := manager.Stats(); got != (IndexStats{Originals: 1, Variants: 1}) {
		t.Fatalf("unexpected index: %+v", got)
	}
	removed, err := manager.InvalidateOriginal(context.Background(), originalRel)
	if err != nil {
		t.Fatalf("InvalidateOriginal: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected one removed variant, got %d", removed)
	}
	assertNotExists(t, variant)
}

func TestBootstrapRemovesOrphanVariant(t *testing.T) {
	manager, baseDir, cacheDir := newTestManager(t)
	// Orphan deletion is skipped while the originals root looks unmounted, so
	// the directory has to hold something unrelated for this case to apply.
	writeTestFile(t, baseDir, "img/present.jpg", []byte("original"))
	variant := writeTestFile(t, cacheDir, "200x200/img/missing.jpg.webp", []byte("resize"))

	if err := bootstrapForTest(manager); err != nil {
		t.Fatalf("bootstrapIndex: %v", err)
	}
	assertNotExists(t, variant)
}

func newTestManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	baseDir := t.TempDir()
	cacheDir := t.TempDir()
	return newManagerForDirs(baseDir, cacheDir), baseDir, cacheDir
}

func newManagerForDirs(baseDir, cacheDir string) *Manager {
	cfg := &config.Config{
		Storage: config.StorageConfig{BaseDir: baseDir, CacheDir: cacheDir},
		Cache:   config.CacheConfig{CheckOriginalsInterval: config.Duration{Duration: time.Minute}},
	}
	return NewManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func writeTestFile(t *testing.T, root, relative string, payload []byte) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func setTestMtime(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func assertNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be absent, stat error: %v", path, err)
	}
}
