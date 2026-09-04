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

func startTestMonitor(t *testing.T, m *Manager) {
	t.Helper()
	m.cfg.Cache.CheckOriginalsInterval.Duration = 10 * time.Millisecond
	m.cfg.Cache.CheckOriginalsWorkers = 2
	m.cfg.Cache.InvalidationLockTimeout.Duration = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	m.StartOriginalMonitor(ctx)
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := m.WaitBackground(stopCtx); err != nil {
			t.Errorf("stop monitor: %v", err)
		}
	})
}

func TestManifestWriteFailureDoesNotStopChecks(t *testing.T) {
	m, base, cache := newTestManager(t)
	source := writeTestFile(t, base, "img/a.jpg", []byte("before"))
	variant := writeTestFile(t, cache, "200x200/img/a.jpg", []byte("resize"))
	// Fail every snapshot write, without preventing removal of cache files.
	obstacle := m.manifestPath() + ".tmp"
	if err := os.Mkdir(obstacle, 0755); err != nil {
		t.Fatal(err)
	}
	startTestMonitor(t, m)
	t.Cleanup(func() {
		if err := os.Remove(obstacle); err != nil {
			t.Error(err)
		}
	})
	waitMonitorCondition(t, func() bool { m.indexMu.RLock(); defer m.indexMu.RUnlock(); return m.indexReady })
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new source content"), 0644); err != nil {
		t.Fatal(err)
	}
	setTestMtime(t, source, info.ModTime())
	waitMonitorCondition(t, func() bool { _, err := os.Stat(variant); return errors.Is(err, os.ErrNotExist) })
}

func TestFailedBootstrapReferenceDoesNotBlockKnownOriginals(t *testing.T) {
	m, base, cache := newTestManager(t)
	bad := writeTestFile(t, base, "img/bad.jpg", []byte("bad"))
	good := writeTestFile(t, base, "img/good.jpg", []byte("before"))
	writeTestFile(t, cache, "200x200/img/bad.jpg", []byte("bad resize"))
	variant := writeTestFile(t, cache, "200x200/img/good.jpg", []byte("good resize"))
	var unavailable atomic.Bool
	unavailable.Store(true)
	m.sourceStat = func(path string) (os.FileInfo, error) {
		if path == bad && unavailable.Load() {
			return nil, os.ErrPermission
		}
		return os.Stat(path)
	}
	startTestMonitor(t, m)
	waitMonitorCondition(t, func() bool { return m.Stats().Originals == 1 })
	m.indexMu.RLock()
	ready := m.indexReady
	m.indexMu.RUnlock()
	if ready {
		t.Fatal("incomplete discovery reported complete")
	}
	if err := os.WriteFile(good, []byte("new good content"), 0644); err != nil {
		t.Fatal(err)
	}
	waitMonitorCondition(t, func() bool { _, err := os.Stat(variant); return errors.Is(err, os.ErrNotExist) })
	unavailable.Store(false)
	waitMonitorCondition(t, func() bool { m.indexMu.RLock(); defer m.indexMu.RUnlock(); return m.indexReady })
}

func TestBootstrapRetriesOnlyFailedReferences(t *testing.T) {
	m, base, cache := newTestManager(t)
	good := writeTestFile(t, base, "img/good.jpg", []byte("good"))
	bad := writeTestFile(t, base, "img/bad.jpg", []byte("bad"))
	writeTestFile(t, cache, "200x200/img/good.jpg", []byte("g"))
	writeTestFile(t, cache, "200x200/img/bad.jpg", []byte("b"))
	var unavailable atomic.Bool
	unavailable.Store(true)
	var goodStats atomic.Int32
	m.sourceStat = func(path string) (os.FileInfo, error) {
		if path == good {
			goodStats.Add(1)
		}
		if path == bad && unavailable.Load() {
			return nil, os.ErrPermission
		}
		return os.Stat(path)
	}
	if err := m.bootstrapIndex(context.Background()); err == nil {
		t.Fatal("expected partial discovery error")
	}
	if m.Stats().Originals != 1 {
		t.Fatal("healthy source was not indexed")
	}
	unavailable.Store(false)
	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := goodStats.Load(); got != 1 {
		t.Fatalf("healthy source rescanned %d times", got)
	}
	if m.Stats().Originals != 2 {
		t.Fatal("retry did not index recovered source")
	}
}

func TestPartialSnapshotPreservesUndiscoveredBaselines(t *testing.T) {
	m, base, cache := newTestManager(t)
	bad := writeTestFile(t, base, "img/bad.jpg", []byte("bad"))
	writeTestFile(t, cache, "200x200/img/bad.jpg", []byte("old"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	previous, err := m.loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	restarted := newManagerForDirs(base, cache)
	restarted.sourceStat = func(path string) (os.FileInfo, error) {
		if path == bad {
			return nil, os.ErrPermission
		}
		return os.Stat(path)
	}
	writeTestFile(t, base, "img/good.jpg", []byte("good"))
	writeTestFile(t, cache, "200x200/img/good.jpg", []byte("new"))
	if err := restarted.bootstrapIndex(context.Background()); err == nil {
		t.Fatal("expected partial discovery")
	}
	if err := restarted.flushManifest(); err != nil {
		t.Fatal(err)
	}
	next, err := restarted.loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if next.Originals["img/bad.jpg"] != previous.Originals["img/bad.jpg"] {
		t.Fatal("lost undiscovered baseline")
	}
	if _, ok := next.Originals["img/good.jpg"]; !ok {
		t.Fatal("known new source not saved")
	}
}

func TestStalledStatDoesNotBlockLaterTicks(t *testing.T) {
	m, base, cache := newTestManager(t)
	slow := writeTestFile(t, base, "img/a-slow.jpg", []byte("slow"))
	good := writeTestFile(t, base, "img/z-good.jpg", []byte("good"))
	writeTestFile(t, cache, "200x200/img/a-slow.jpg", []byte("slow resize"))
	variant := writeTestFile(t, cache, "200x200/img/z-good.jpg", []byte("good resize"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	entered, unblock := make(chan struct{}), make(chan struct{})
	var slowCalls, goodCalls atomic.Int32
	var once sync.Once
	m.sourceStat = func(path string) (os.FileInfo, error) {
		if path == slow {
			slowCalls.Add(1)
			once.Do(func() { close(entered) })
			<-unblock
		}
		if path == good {
			goodCalls.Add(1)
		}
		return os.Stat(path)
	}
	startTestMonitor(t, m)
	// Release the deliberately non-cancellable fake syscall before shutdown waits.
	t.Cleanup(func() { close(unblock) })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow stat never started")
	}
	// Healthy source is revisited on later ticks while the original call is still blocked.
	waitMonitorCondition(t, func() bool { return goodCalls.Load() >= 3 })
	if slowCalls.Load() != 1 {
		t.Fatal("duplicate in-flight stat/goroutine for slow key")
	}
	if err := os.WriteFile(good, []byte("replacement good source"), 0644); err != nil {
		t.Fatal(err)
	}
	waitMonitorCondition(t, func() bool { _, err := os.Stat(variant); return errors.Is(err, os.ErrNotExist) })
}

func TestBusyOriginalDoesNotBlockOtherInvalidations(t *testing.T) {
	m, base, cache := newTestManager(t)
	slow := writeTestFile(t, base, "img/a-busy.jpg", []byte("old"))
	good := writeTestFile(t, base, "img/z-good.jpg", []byte("old"))
	busyVariant := writeTestFile(t, cache, "200x200/img/a-busy.jpg", []byte("resize"))
	goodVariant := writeTestFile(t, cache, "200x200/img/z-good.jpg", []byte("resize"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	unlock := m.LockOriginal("img/a-busy.jpg")
	var once sync.Once
	startTestMonitor(t, m)
	t.Cleanup(func() { once.Do(unlock) })
	for _, path := range []string{slow, good} {
		if err := os.WriteFile(path, []byte("new content"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	waitMonitorCondition(t, func() bool { _, err := os.Stat(goodVariant); return errors.Is(err, os.ErrNotExist) })
	if _, err := os.Stat(busyVariant); err != nil {
		t.Fatal("busy variant was removed without its lock")
	}
	once.Do(unlock)
	waitMonitorCondition(t, func() bool { _, err := os.Stat(busyVariant); return errors.Is(err, os.ErrNotExist) })
}

func TestBootstrapDirectoryFailureIsRetried(t *testing.T) {
	m, base, cache := newTestManager(t)
	writeTestFile(t, base, "img/good.jpg", []byte("good"))
	writeTestFile(t, base, "img/bad.jpg", []byte("bad"))
	writeTestFile(t, cache, "200x200/img/good.jpg", []byte("good"))
	writeTestFile(t, cache, "400x400/img/bad.jpg", []byte("bad"))
	blocked := filepath.Join(cache, "400x400")
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0755) })
	if _, err := os.ReadDir(blocked); err == nil {
		t.Skip("filesystem/user bypasses directory permission checks")
	}
	if err := m.bootstrapIndex(context.Background()); err == nil {
		t.Fatal("expected directory error")
	}
	if got := m.Stats(); got.Originals != 1 {
		t.Fatalf("accessible sibling lost: %+v", got)
	}
	if err := os.Chmod(blocked, 0755); err != nil {
		t.Fatal(err)
	}
	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Stats(); got.Originals != 2 {
		t.Fatalf("directory retry failed: %+v", got)
	}
}
