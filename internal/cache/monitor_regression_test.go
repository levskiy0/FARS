package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewVariantCannotHideChangedOriginal(t *testing.T) {
	m, base, cache := newTestManager(t)
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("old"))
	old := writeTestFile(t, cache, "200x200/"+rel, []byte("old resize"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("a changed original"), 0644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(cache, "400x400", rel)
	if err := m.Write(newer, rel, info, []byte("new resize")); err != nil {
		t.Fatal(err)
	}
	if m.IsFresh(old, info) {
		t.Fatal("old resize incorrectly became fresh")
	}
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, old)
	assertNotExists(t, newer)
}

func TestFailedInvalidationRemainsPendingAndRetries(t *testing.T) {
	m, base, cache := newTestManager(t)
	rel := "img/photo.jpg"
	writeTestFile(t, base, rel, []byte("unchanged"))
	variant := writeTestFile(t, cache, "200x200/"+rel, []byte("resize"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	// A nonempty directory forces removal to fail, independent of user privileges.
	if err := os.Remove(variant); err != nil {
		t.Fatal(err)
	}
	child := writeTestFile(t, variant, "obstacle", []byte("x"))
	if _, err := m.InvalidateOriginal(context.Background(), rel); err == nil {
		t.Fatal("expected removal error")
	}
	if got := m.Stats(); got.Variants != 1 {
		t.Fatalf("lost failed variant: %+v", got)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(variant); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(variant, []byte("resize"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, variant)
}

func TestCancelledInvalidationRetainsIndex(t *testing.T) {
	m, base, cache := newTestManager(t)
	rel := "img/photo.jpg"
	writeTestFile(t, base, rel, []byte("original"))
	variant := writeTestFile(t, cache, "200x200/"+rel, []byte("resize"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.InvalidateOriginal(ctx, rel); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if got := m.Stats(); got.Variants != 1 {
		t.Fatalf("lost variant on cancellation: %+v", got)
	}
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, variant)
}

func TestWriteRejectsSourceChangedDuringResize(t *testing.T) {
	m, base, cache := newTestManager(t)
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("before"))
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("after replacement"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache, "200x200", rel)
	if err := m.Write(path, rel, info, []byte("stale resize")); err == nil {
		t.Fatal("published stale result")
	}
	assertNotExists(t, path)
}

func TestFallbackInvalidationPreservesExactDoubleExtensionSource(t *testing.T) {
	m, base, cache := newTestManager(t)
	writeTestFile(t, base, "img/photo.jpg", []byte("base"))
	writeTestFile(t, base, "img/photo.jpg.webp", []byte("independent source"))
	first := writeTestFile(t, cache, "200x200/img/photo.jpg", []byte("base resize"))
	second := writeTestFile(t, cache, "200x200/img/photo.jpg.webp", []byte("independent resize"))
	if _, err := m.InvalidateOriginal(context.Background(), "img/photo.jpg"); err != nil {
		t.Fatal(err)
	}
	assertNotExists(t, first)
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("removed independent source variant: %v", err)
	}
}

func TestOriginalMonitorStartsChecksAndStops(t *testing.T) {
	m, base, cache := newTestManager(t)
	m.cfg.Cache.CheckOriginalsInterval.Duration = 5 * time.Millisecond
	source := writeTestFile(t, base, "img/photo.jpg", []byte("old"))
	variant := writeTestFile(t, cache, "200x200/img/photo.jpg", []byte("resize"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartBackground(ctx)
	t.Cleanup(func() {
		cancel()
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := m.WaitBackground(stopCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	waitMonitorCondition(t, func() bool {
		m.indexMu.RLock()
		defer m.indexMu.RUnlock()
		return m.indexReady
	})
	if err := os.WriteFile(source, []byte("changed original"), 0644); err != nil {
		t.Fatal(err)
	}
	waitMonitorCondition(t, func() bool { _, err := os.Stat(variant); return errors.Is(err, os.ErrNotExist) })
}

func waitMonitorCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("monitor did not reach expected state")
		case <-poll.C:
		}
	}
}

func TestUnchangedMonitorDoesNotRewriteManifest(t *testing.T) {
	m, base, cache := newTestManager(t)
	writeTestFile(t, base, "img/photo.jpg", []byte("original"))
	writeTestFile(t, cache, "200x200/img/photo.jpg", []byte("resize"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	sentinel := time.Unix(1700000000, 0)
	setTestMtime(t, m.manifestPath(), sentinel)
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.flushManifest(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(m.manifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(sentinel) {
		t.Fatal("unchanged index caused a disk write")
	}
}
