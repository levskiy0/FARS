package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func markChangedOriginals(t *testing.T, m *Manager) {
	t.Helper()
	for _, key := range m.originalKeys() {
		if err := m.checkOriginal(context.Background(), key); err != nil {
			t.Fatalf("checkOriginal(%s): %v", key, err)
		}
	}
}

// TestHotOriginalLeavesTheInvalidationQueue is the regenerate-delete loop: while
// the doomed variants of a changed source are still queued, requests keep
// publishing new ones. Only the variants that predate the change may be removed,
// and the queue has to drain even though it keeps receiving new variants.
func TestHotOriginalLeavesTheInvalidationQueue(t *testing.T) {
	m, base, cache := newTestManager(t)
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("source v0"))
	stale := []string{}
	for _, geometry := range []string{"100x100", "200x200", "300x300"} {
		stale = append(stale, writeTestFile(t, cache, geometry+"/"+rel, []byte("resize v0")))
	}
	if err := bootstrapForTest(m); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	for round := 1; round <= 3; round++ {
		content := fmt.Sprintf("source v%d, grown by %d bytes", round, round)
		if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		markChangedOriginals(t, m)
		if keys := m.pendingKeys(); len(keys) == 0 {
			t.Fatalf("round %d: changed source did not queue an invalidation", round)
		}

		// A request regenerates from the source that is current right now,
		// while the previous variants are still queued for deletion.
		info, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		fresh := filepath.Join(cache, fmt.Sprintf("%d0x%d0", round, round), filepath.FromSlash(rel))
		payload := []byte(fmt.Sprintf("resize v%d", round))
		releaseOriginal := m.LockOriginal(rel)
		releaseCache := m.LockCache(fresh)
		err = m.Write(fresh, rel, info, payload)
		releaseCache()
		releaseOriginal()
		if err != nil {
			t.Fatalf("round %d: Write: %v", round, err)
		}

		if err := m.drainPendingOnce(context.Background()); err != nil {
			t.Fatalf("round %d: drainPendingOnce: %v", round, err)
		}
		for _, path := range stale {
			assertNotExists(t, path)
		}
		if data, err := os.ReadFile(fresh); err != nil || string(data) != string(payload) {
			t.Fatalf("round %d: variant published from the current source was deleted: %q, %v", round, data, err)
		}
		if keys := m.pendingKeys(); len(keys) != 0 {
			t.Fatalf("round %d: invalidation queue never drained: %v", round, keys)
		}
		// The variant published this round predates the next change.
		stale = append(stale, fresh)
	}

	// A quiet pass over an unchanged source must not remove anything further.
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	survivor := stale[len(stale)-1]
	if _, err := os.Stat(survivor); err != nil {
		t.Fatalf("settled variant removed by a later scan: %v", err)
	}
}

// TestDoomedSetSpansGeometriesOfOneOriginal checks the set is per variant, not
// per original: every variant registered before the change goes, and a variant
// of a different original is untouched.
func TestDoomedSetSpansGeometriesOfOneOriginal(t *testing.T) {
	m, base, cache := newTestManager(t)
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("source"))
	writeTestFile(t, base, "img/other.jpg", []byte("other source"))
	doomed := []string{
		writeTestFile(t, cache, "100x100/"+rel, []byte("a")),
		writeTestFile(t, cache, "200x200/"+rel+".webp", []byte("b")),
		writeTestFile(t, cache, "0x300/"+rel+".avif", []byte("c")),
	}
	untouched := writeTestFile(t, cache, "100x100/img/other.jpg", []byte("keep"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := os.WriteFile(source, []byte("source, replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	markChangedOriginals(t, m)

	m.indexMu.RLock()
	entry := m.originals[rel]
	var doomedCount int
	if entry != nil {
		doomedCount = len(entry.Doomed)
	}
	m.indexMu.RUnlock()
	if doomedCount != len(doomed) {
		t.Fatalf("doomed set holds %d variants, want %d", doomedCount, len(doomed))
	}

	if err := m.drainPendingOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range doomed {
		assertNotExists(t, path)
	}
	if data, err := os.ReadFile(untouched); err != nil || string(data) != "keep" {
		t.Fatalf("variant of an unrelated original removed: %q, %v", data, err)
	}
}
