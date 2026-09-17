package cache

import (
	"context"
	"os"
	"testing"
)

func TestQueuedOrphanCannotDeleteRegeneratedResize(t *testing.T) {
	m, base, cacheDir := newTestManager(t)
	rel := "img/photo.jpg"
	// Orphan deletion is skipped while the originals root looks unmounted.
	writeTestFile(t, base, "img/present.jpg", []byte("unrelated original"))
	path := writeTestFile(t, cacheDir, "200x200/"+rel, []byte("orphan"))
	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Keep a previously dispatched key to reproduce deletion delayed behind a writer.
	keys := m.pendingKeys()
	if len(keys) != 1 {
		t.Fatalf("expected orphan task, got %v", keys)
	}
	source := writeTestFile(t, base, rel, []byte("restored source"))
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	unlockOriginal := m.LockOriginal(rel)
	unlockCache := m.LockCache(path)
	err = m.Write(path, rel, info, []byte("fresh resize"))
	unlockCache()
	unlockOriginal()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.invalidatePending(context.Background(), keys[0]); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "fresh resize" {
		t.Fatalf("stale orphan task deleted regenerated resize: %q, %v", data, err)
	}
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("resize lost after next scan: %v", err)
	}
}
