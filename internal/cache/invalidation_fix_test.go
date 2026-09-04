package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBootstrapRetiredHistoryRecovery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{name: "retry discovery"},
		{name: "restart during discovery", restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, base, cacheDir := newTestManager(t)
			rel := "img/photo.jpg"
			source := writeTestFile(t, base, rel, []byte("old"))
			mtime := time.Now().Add(-time.Hour)
			setTestMtime(t, source, mtime)
			initial, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			first := writeTestFile(t, cacheDir, "200x200/"+rel, []byte("old JPEG"))
			late := writeTestFile(t, cacheDir, "400x400/"+rel+".webp", []byte("old WebP"))
			var inaccessible atomic.Bool
			inaccessible.Store(true)
			m.sourceStat = func(path string) (os.FileInfo, error) {
				if path == filepath.Join(base, rel+".webp") && inaccessible.Load() {
					return nil, os.ErrPermission
				}
				return os.Stat(path)
			}
			if err := m.bootstrapIndex(context.Background()); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("expected partial discovery, got %v", err)
			}
			if err := os.WriteFile(source, []byte("changed source"), 0o644); err != nil {
				t.Fatal(err)
			}
			setTestMtime(t, source, mtime)
			if err := m.checkOriginalsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertNotExists(t, first)
			if got := m.Stats(); got != (IndexStats{}) {
				t.Fatalf("retired history leaked into active index: %+v", got)
			}
			if err := m.flushManifest(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := m.loadManifest()
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Originals[rel] != signatureFromInfo(initial) || !snapshot.Pending[rel] {
				t.Fatalf("partial snapshot lost invalidation history: %+v", snapshot)
			}

			recovered := m
			if tc.restart {
				recovered = newManagerForDirs(base, cacheDir)
			} else {
				inaccessible.Store(false)
			}
			if err := bootstrapForTest(recovered); err != nil {
				t.Fatal(err)
			}
			assertNotExists(t, late)
			if got := recovered.Stats(); got != (IndexStats{}) {
				t.Fatalf("index not drained: %+v", got)
			}
			if len(recovered.bootstrapRetired) != 0 {
				t.Fatal("bootstrap history retained after discovery completed")
			}
			snapshot, err = recovered.loadManifest()
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := snapshot.Originals[rel]; exists || snapshot.Pending[rel] {
				t.Fatal("completed snapshot still contains retired source")
			}
			writeInvalidationReviewVariant(t, recovered, source, rel, first, "fresh")
			if err := recovered.checkOriginalsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(first); err != nil || string(data) != "fresh" {
				t.Fatalf("old history invalidated new generation: %q, %v", data, err)
			}
		})
	}
}

func TestCleanupPreservesRegeneratedOrphan(t *testing.T) {
	m, base, cacheDir := newTestManager(t)
	rel := "img/photo.jpg"
	source := filepath.Join(base, rel)
	variant := writeTestFile(t, cacheDir, "200x200/"+rel, []byte("orphan"))
	entered, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	m.sourceStat = func(path string) (os.FileInfo, error) {
		info, err := os.Stat(path)
		if path == source {
			close(entered)
			<-resume
		}
		return info, err
	}
	finish := runInvalidationReviewJob(t, m.cleanupOnce, release)
	waitInvalidationReviewSignal(t, entered)
	writeTestFile(t, base, rel, []byte("restored original"))
	writeInvalidationReviewVariant(t, m, source, rel, variant, "fresh")
	release()
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(variant); err != nil || string(data) != "fresh" {
		t.Fatalf("cleanup removed regenerated orphan: %q, %v", data, err)
	}
}

func TestCleanupRemovalGuard(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
		missing bool
	}{
		{name: "unchanged file is removed"},
		{name: "atomic replacement with same size and mtime survives", replace: true},
		{name: "already removed is harmless", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _, cacheDir := newTestManager(t)
			path := writeTestFile(t, cacheDir, "200x200/img/a.jpg", []byte("old"))
			mtime := time.Now().Add(-time.Hour)
			setTestMtime(t, path, mtime)
			observed, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.replace {
				replacement := writeTestFile(t, cacheDir, "replacement.tmp", []byte("new"))
				setTestMtime(t, replacement, mtime)
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			}
			if tc.missing {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			var stats cleanupStats
			removed, err := m.removeCacheFileIfUnchanged(context.Background(), path, observed, &stats)
			wantRemoved := !tc.replace && !tc.missing
			if err != nil || removed != wantRemoved {
				t.Fatalf("removed=%v, want=%v, err=%v", removed, wantRemoved, err)
			}
			if tc.replace {
				if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
					t.Fatalf("replacement removed: %q, %v", data, err)
				}
			} else {
				assertNotExists(t, path)
			}
			wantFiles := 0
			if wantRemoved {
				wantFiles = 1
			}
			if stats.files != wantFiles {
				t.Fatalf("incorrect removal stats: %+v", stats)
			}
		})
	}
}

func TestDisabledMonitorDoesNotRetainBootstrapHistory(t *testing.T) {
	m, base, cacheDir := newTestManager(t)
	m.cfg.Cache.CheckOriginalsInterval.Duration = 0
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("original"))
	variant := filepath.Join(cacheDir, "200x200", rel)
	writeInvalidationReviewVariant(t, m, source, rel, variant, "resize")
	if _, err := m.InvalidateOriginal(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
	if len(m.bootstrapRetired) != 0 || m.Stats() != (IndexStats{}) {
		t.Fatal("disabled monitor retained an unused source")
	}
}
