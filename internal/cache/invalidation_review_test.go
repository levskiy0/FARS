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

// Discovery can be incomplete while the independent check/deletion pools run.
// A changed source must invalidate variants discovered later in the same bootstrap.
func TestInvalidationReviewChangeDuringBootstrap(t *testing.T) {
	m, base, cacheDir := newTestManager(t)
	m.cfg.Cache.CheckOriginalsWorkers = 2
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("old"))
	mtime := time.Now().Add(-time.Hour)
	setTestMtime(t, source, mtime)
	first := writeTestFile(t, cacheDir, "200x200/"+rel, []byte("old JPEG"))
	deferred := writeTestFile(t, cacheDir, "400x400/"+rel+".webp", []byte("old WebP"))

	entered, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	m.sourceStat = func(path string) (os.FileInfo, error) {
		if path == filepath.Join(base, rel+".webp") {
			close(entered)
			<-resume
		}
		return os.Stat(path)
	}
	finish := runInvalidationReviewJob(t, m.bootstrapIndex, release)
	waitInvalidationReviewSignal(t, entered)
	waitMonitorCondition(t, func() bool { return m.Stats().Variants == 1 })

	// The metadata change is observed while only the JPEG variant is indexed.
	if err := os.WriteFile(source, []byte("replacement source"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTestMtime(t, source, mtime)
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, first)

	release()
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.checkOriginalsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	assertNotExists(t, deferred)
}

// Cleanup must revalidate the deletion decision after a concurrent writer has
// published new bytes, not delete a fresh file based on its predecessor's mtime.
func TestInvalidationReviewCleanupPreservesConcurrentRegeneration(t *testing.T) {
	m, base, cacheDir := newTestManager(t)
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("source"))
	setTestMtime(t, source, time.Now().Add(-time.Hour))
	variant := writeTestFile(t, cacheDir, "200x200/"+rel, []byte("expired resize"))
	setTestMtime(t, variant, time.Now().Add(-2*time.Hour))

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
	writeInvalidationReviewVariant(t, m, source, rel, variant, "fresh resize")
	release()
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(variant)
	if err != nil || string(data) != "fresh resize" {
		t.Fatalf("cleanup deleted concurrently regenerated resize: data=%q err=%v", data, err)
	}
}

func TestInvalidationReviewPendingSurvivesRestart(t *testing.T) {
	m, base, cacheDir := newTestManager(t)
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("unchanged original"))
	variant := writeTestFile(t, cacheDir, "200x200/"+rel, []byte("resize"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	unlock := sync.OnceFunc(m.LockCache(variant))
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.InvalidateOriginal(ctx, rel); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected blocked removal deadline, got %v", err)
	}
	unlock()
	if err := m.flushManifest(); err != nil {
		t.Fatal(err)
	}

	restarted := newManagerForDirs(base, cacheDir)
	if err := bootstrapForTest(restarted); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, variant)
	if data, err := os.ReadFile(source); err != nil || string(data) != "unchanged original" {
		t.Fatalf("original changed: %q, %v", data, err)
	}
}

func TestInvalidationReviewManualRetryAfterDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		indexed bool
	}{
		{name: "before bootstrap"},
		{name: "after bootstrap", indexed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, base, cacheDir := newTestManager(t)
			rel := "img/photo.jpg"
			writeTestFile(t, base, rel, []byte("original"))
			variant := writeTestFile(t, cacheDir, "200x200/"+rel, []byte("resize"))
			if tc.indexed {
				if err := bootstrapForTest(m); err != nil {
					t.Fatal(err)
				}
			}
			unlock := sync.OnceFunc(m.LockCache(variant))
			defer unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := m.InvalidateOriginal(ctx, rel); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected deadline, got %v", err)
			}
			unlock()
			count, err := m.InvalidateOriginal(context.Background(), rel)
			if err != nil || count != 1 {
				t.Fatalf("retry: removed=%d, err=%v", count, err)
			}
			assertNotExists(t, variant)
		})
	}
}

func TestInvalidationReviewTemporaryStatFailureRetries(t *testing.T) {
	m, base, cacheDir := newTestManager(t)
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("old"))
	first := writeTestFile(t, cacheDir, "200x200/"+rel, []byte("old JPEG"))
	second := writeTestFile(t, cacheDir, "400x400/"+rel+".webp", []byte("old WebP"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("changed original"), 0o644); err != nil {
		t.Fatal(err)
	}
	var unavailable atomic.Bool
	unavailable.Store(true)
	m.sourceStat = func(path string) (os.FileInfo, error) {
		if path == source && unavailable.Load() {
			return nil, os.ErrPermission
		}
		return os.Stat(path)
	}
	if err := m.checkOriginalsOnce(context.Background()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected stat error, got %v", err)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("transient stat error removed resize %s: %v", path, err)
		}
	}
	unavailable.Store(false)
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, first)
	assertNotExists(t, second)
}

func TestInvalidationReviewDoesNotStatUnreferencedOriginal(t *testing.T) {
	m, base, cacheDir := newTestManager(t)
	source := writeTestFile(t, base, "img/used.jpg", []byte("used"))
	writeTestFile(t, base, "img/unused.jpg", []byte("unused"))
	variant := writeTestFile(t, cacheDir, "200x200/img/used.jpg", []byte("resize"))
	var unrelatedStats atomic.Int32
	m.sourceStat = func(path string) (os.FileInfo, error) {
		if path != source {
			unrelatedStats.Add(1)
		}
		return os.Stat(path)
	}
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("changed used source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, variant)
	if got := unrelatedStats.Load(); got != 0 {
		t.Fatalf("stat called on unreferenced originals %d times", got)
	}
}

func writeInvalidationReviewVariant(t *testing.T, m *Manager, source, rel, path, payload string) {
	t.Helper()
	unlockSource := m.LockOriginal(rel)
	defer unlockSource()
	unlockCache := m.LockCache(path)
	defer unlockCache()
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Write(path, rel, info, []byte(payload)); err != nil {
		t.Fatal(err)
	}
}

// Joins every test goroutine, including on assertion failures. Channels establish
// ordering; timeouts only bound a broken/hung test and do not determine the race.
func runInvalidationReviewJob(t *testing.T, run func(context.Context) error, unblock func()) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result error
	go func() {
		defer close(done)
		result = run(ctx)
	}()
	t.Cleanup(func() {
		unblock()
		cancel()
		waitInvalidationReviewSignal(t, done)
	})
	return func() error {
		waitInvalidationReviewSignal(t, done)
		return result
	}
}

func waitInvalidationReviewSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatal("invalidation review task did not reach synchronization point")
	}
}
