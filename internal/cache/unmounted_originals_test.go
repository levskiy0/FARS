package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resetRootProbe expires every cached probe answer, so a test can move between
// "volume gone" and "volume back" without waiting out rootProbeTTL.
func resetRootProbe(m *Manager) {
	m.probeMu.Lock()
	clear(m.probes)
	m.probeMu.Unlock()
}

// emptyOriginalsRoot produces the two shapes an unattached bind mount takes:
// an empty directory, or no directory at all.
func emptyOriginalsRoot(t *testing.T, base string, missing bool) {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(base, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	if missing {
		if err := os.Remove(base); err != nil {
			t.Fatal(err)
		}
	}
}

func restoreOriginalsRoot(t *testing.T, base string) {
	t.Helper()
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestCleanupSkipsOrphanRemovalWhileOriginalsRootIsEmpty reproduces the startup
// wipe: with the originals bind mount unattached every cached file looks like an
// orphan, and the whole cache used to be deleted in one pass.
func TestCleanupSkipsOrphanRemovalWhileOriginalsRootIsEmpty(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "empty base_dir"
		if missing {
			name = "missing base_dir"
		}
		t.Run(name, func(t *testing.T) {
			m, base, cache := newTestManager(t)
			m.cfg.Cache.TTL.Duration = 30 * 24 * time.Hour
			variant := writeTestFile(t, cache, "200x200/img/photo.jpg", []byte("resize"))
			setTestMtime(t, variant, time.Now())
			emptyOriginalsRoot(t, base, missing)

			if err := m.cleanupOnce(context.Background()); err != nil {
				t.Fatalf("cleanupOnce: %v", err)
			}
			if data, err := os.ReadFile(variant); err != nil || string(data) != "resize" {
				t.Fatalf("fresh cache entry deleted while the originals volume was unavailable: %q, %v", data, err)
			}

			// Once the volume is back and this original really is absent, the
			// entry is an orphan again and normal cleanup removes it.
			restoreOriginalsRoot(t, base)
			writeTestFile(t, base, "img/other.jpg", []byte("original"))
			if err := m.cleanupOnce(context.Background()); err != nil {
				t.Fatalf("second cleanupOnce: %v", err)
			}
			assertNotExists(t, variant)
		})
	}
}

// TestCleanupEvictsByTTLWhileOrphanRemovalIsPaused keeps the pause narrow: only
// orphan deletion waits for the originals volume, retention does not.
func TestCleanupEvictsByTTLWhileOrphanRemovalIsPaused(t *testing.T) {
	m, base, cache := newTestManager(t)
	m.cfg.Cache.TTL.Duration = time.Hour
	fresh := writeTestFile(t, cache, "200x200/img/fresh.jpg", []byte("fresh resize"))
	stale := writeTestFile(t, cache, "200x200/img/stale.jpg", []byte("stale resize"))
	setTestMtime(t, fresh, time.Now())
	setTestMtime(t, stale, time.Now().Add(-48*time.Hour))
	emptyOriginalsRoot(t, base, false)

	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	assertNotExists(t, stale)
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh entry evicted with the expired one: %v", err)
	}
}

// TestBootstrapDefersOrphanQueueingWhileOriginalsRootIsEmpty is the same guard
// on the discovery path: orphan tasks must not be queued from an unmounted
// volume, and discovery must stay incomplete until it returns.
func TestBootstrapDefersOrphanQueueingWhileOriginalsRootIsEmpty(t *testing.T) {
	m, base, cache := newTestManager(t)
	variant := writeTestFile(t, cache, "200x200/img/photo.jpg", []byte("resize"))
	emptyOriginalsRoot(t, base, false)

	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatalf("bootstrapIndex: %v", err)
	}
	if keys := m.pendingKeys(); len(keys) != 0 {
		t.Fatalf("queued orphan deletion from an unavailable originals volume: %v", keys)
	}
	m.indexMu.RLock()
	ready := m.indexReady
	m.indexMu.RUnlock()
	if ready {
		t.Fatal("discovery reported complete while cache references were deferred")
	}
	if err := m.drainPendingOnce(context.Background()); err != nil {
		t.Fatalf("drainPendingOnce: %v", err)
	}
	if data, err := os.ReadFile(variant); err != nil || string(data) != "resize" {
		t.Fatalf("cache entry deleted during bootstrap with an unavailable originals volume: %q, %v", data, err)
	}

	writeTestFile(t, base, "img/other.jpg", []byte("original"))
	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatalf("second bootstrapIndex: %v", err)
	}
	if err := m.drainPendingOnce(context.Background()); err != nil {
		t.Fatalf("second drainPendingOnce: %v", err)
	}
	assertNotExists(t, variant)
	m.indexMu.RLock()
	ready = m.indexReady
	m.indexMu.RUnlock()
	if !ready {
		t.Fatal("discovery never completed after the originals volume returned")
	}
}

// TestCheckOriginalIgnoresMissingSourceWhileRootIsEmpty guards the per-key path:
// a source that vanished with its whole volume is an unmount, not a deletion.
func TestCheckOriginalIgnoresMissingSourceWhileRootIsEmpty(t *testing.T) {
	m, base, cache := newTestManager(t)
	source := writeTestFile(t, base, "photo.jpg", []byte("original"))
	variant := writeTestFile(t, cache, "200x200/photo.jpg", []byte("resize"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := m.Stats(); got != (IndexStats{Originals: 1, Variants: 1}) {
		t.Fatalf("unexpected index after bootstrap: %+v", got)
	}

	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	resetRootProbe(m)
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatalf("checkOriginalsOnce: %v", err)
	}
	if keys := m.pendingKeys(); len(keys) != 0 {
		t.Fatalf("queued invalidation from an unavailable originals volume: %v", keys)
	}
	if data, err := os.ReadFile(variant); err != nil || string(data) != "resize" {
		t.Fatalf("variant invalidated because the originals root was empty: %q, %v", data, err)
	}

	// Populated root, and this one original really was deleted.
	writeTestFile(t, base, "other.jpg", []byte("unrelated original"))
	resetRootProbe(m)
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatalf("second checkOriginalsOnce: %v", err)
	}
	assertNotExists(t, variant)
}

// TestOriginalsRootProbeIsCached pins the probe's one-second memory: the monitor
// asks it per key, so it must not readdir the volume per key.
func TestOriginalsRootProbeIsCached(t *testing.T) {
	m, base, _ := newTestManager(t)
	emptyOriginalsRoot(t, base, false)
	if m.originalsRootPopulatedCached() {
		t.Fatal("empty originals root reported as populated")
	}
	writeTestFile(t, base, "img/a.jpg", []byte("original"))
	if m.originalsRootPopulatedCached() {
		t.Fatal("probe result was not cached within its TTL")
	}
	resetRootProbe(m)
	if !m.originalsRootPopulatedCached() {
		t.Fatal("probe never observed the restored originals root")
	}
}
