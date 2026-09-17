package cache

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestBootstrapDropsVanishedDirectory covers the race with cleanup: pruning an
// empty geometry directory while discovery walks it must not keep that path
// queued for the lifetime of the process.
func TestBootstrapDropsVanishedDirectory(t *testing.T) {
	m, base, cache := newTestManager(t)
	writeTestFile(t, base, "img/photo.jpg", []byte("original"))
	writeTestFile(t, cache, "200x200/img/photo.jpg", []byte("resize"))
	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatalf("first bootstrapIndex: %v", err)
	}

	pruned := filepath.Join(cache, "400x400")
	m.bootstrapMu.Lock()
	m.bootstrapState.directories[pruned] = struct{}{}
	m.bootstrapMu.Unlock()
	m.indexMu.Lock()
	m.indexReady = false
	m.indexMu.Unlock()

	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatalf("a directory that no longer exists was reported as a failure: %v", err)
	}
	m.bootstrapMu.Lock()
	_, requeued := m.bootstrapState.directories[pruned]
	failures := m.bootstrapState.failures[pruned]
	m.bootstrapMu.Unlock()
	if requeued {
		t.Fatal("a directory removed by cleanup was re-queued for discovery")
	}
	if failures != 0 {
		t.Fatalf("a vanished directory was counted as %d failures", failures)
	}
	m.indexMu.RLock()
	ready := m.indexReady
	m.indexMu.RUnlock()
	if !ready {
		t.Fatal("discovery never completed after a directory vanished")
	}
}

// TestAbandonedReferenceKeepsDiscoveryIncomplete pins the cost of giving up on
// a path: discovery may stop retrying it every interval, but it must not call
// itself complete. Declaring completeness clears the Unknown markers, releases
// the retired entries and stops the manifest from carrying the baselines of the
// subtree that was never read - a permanent degradation caused by a transient
// permission problem.
func TestAbandonedReferenceKeepsDiscoveryIncomplete(t *testing.T) {
	m, base, cache := newTestManager(t)
	bad := writeTestFile(t, base, "img/bad.jpg", []byte("bad"))
	writeTestFile(t, base, "img/good.jpg", []byte("good"))
	writeTestFile(t, cache, "200x200/img/bad.jpg", []byte("bad resize"))
	writeTestFile(t, cache, "200x200/img/good.jpg", []byte("good resize"))
	var blocked atomic.Bool
	blocked.Store(true)
	m.sourceStat = func(path string) (os.FileInfo, error) {
		if path == bad && blocked.Load() {
			return nil, os.ErrPermission
		}
		return os.Stat(path)
	}

	for attempt := 0; attempt < maxBootstrapPathFailures; attempt++ {
		_ = m.bootstrapIndex(context.Background())
	}
	m.indexMu.RLock()
	ready := m.indexReady
	m.indexMu.RUnlock()
	if ready {
		t.Fatal("discovery reported complete although one subtree was never read")
	}
	m.bootstrapMu.Lock()
	queued := len(m.bootstrapState.references)
	schedule, backingOff := m.bootstrapState.abandoned["img/bad.jpg"]
	m.bootstrapMu.Unlock()
	if queued != 1 {
		t.Fatalf("the unreadable reference was dropped instead of deferred: %d queued", queued)
	}
	if !backingOff || schedule.backoff <= 0 {
		t.Fatal("the unreadable reference was not put on a back-off")
	}
	if got := m.Stats(); got.Originals != 1 {
		t.Fatalf("the healthy source was lost along with the failing one: %+v", got)
	}

	// The failing path is not retried while its back-off runs.
	before := m.Stats()
	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatalf("a deferred reference was retried during its back-off: %v", err)
	}
	if got := m.Stats(); got != before {
		t.Fatalf("index changed while the reference was backing off: %+v", got)
	}

	// Once the back-off is due and the permission problem is gone, discovery
	// picks the subtree up again and only then completes.
	blocked.Store(false)
	m.bootstrapMu.Lock()
	m.bootstrapState.abandoned["img/bad.jpg"] = retrySchedule{at: time.Now().Add(-time.Second), backoff: schedule.backoff}
	m.bootstrapMu.Unlock()
	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatalf("retry after back-off: %v", err)
	}
	m.indexMu.RLock()
	ready = m.indexReady
	m.indexMu.RUnlock()
	if !ready {
		t.Fatal("discovery never completed after the failing path recovered")
	}
	if got := m.Stats(); got.Originals != 2 {
		t.Fatalf("recovered source was not indexed: %+v", got)
	}
}

// TestAbandonedSubtreeKeepsItsBaselinesInTheManifest is the concrete damage of a
// premature "complete": flushManifest stops merging the previous manifest once
// discovery is ready, so the baselines of the subtree nobody could read are lost
// and its variants can never be judged stale again.
func TestAbandonedSubtreeKeepsItsBaselinesInTheManifest(t *testing.T) {
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
	for attempt := 0; attempt < maxBootstrapPathFailures; attempt++ {
		_ = restarted.bootstrapIndex(context.Background())
	}
	if err := restarted.flushManifest(); err != nil {
		t.Fatal(err)
	}
	next, err := restarted.loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if next.Originals["img/bad.jpg"] != previous.Originals["img/bad.jpg"] {
		t.Fatal("baseline of the abandoned subtree was dropped from the manifest")
	}
}
