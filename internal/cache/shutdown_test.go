package cache

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestWaitBackgroundFlushesManifestDespiteStuckWorker pins the shutdown
// contract: a worker blocked in a syscall on a hung volume must not cost the
// whole inventory learned since the last flush.
func TestWaitBackgroundFlushesManifestDespiteStuckWorker(t *testing.T) {
	m, base, cache := newTestManager(t)
	writeTestFile(t, base, "img/photo.jpg", []byte("original"))
	writeTestFile(t, cache, "200x200/img/photo.jpg", []byte("resize"))
	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatalf("bootstrapIndex: %v", err)
	}
	if _, err := os.Stat(m.manifestPath()); !os.IsNotExist(err) {
		t.Fatalf("manifest already on disk before the flush: %v", err)
	}

	stuck := make(chan struct{})
	m.background.Add(1)
	go func() {
		defer m.background.Done()
		<-stuck
	}()
	t.Cleanup(func() { close(stuck) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := m.WaitBackground(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected the deadline to be reported, got %v", err)
	}

	manifest, err := m.loadManifest()
	if err != nil {
		t.Fatalf("manifest was not written while a worker was stuck: %v", err)
	}
	if _, ok := manifest.Originals["img/photo.jpg"]; !ok {
		t.Fatalf("inventory lost on shutdown: %+v", manifest.Originals)
	}
}

func TestWaitBackgroundFlushesManifestOnCleanShutdown(t *testing.T) {
	m, base, cache := newTestManager(t)
	writeTestFile(t, base, "img/photo.jpg", []byte("original"))
	writeTestFile(t, cache, "200x200/img/photo.jpg", []byte("resize"))
	if err := m.bootstrapIndex(context.Background()); err != nil {
		t.Fatalf("bootstrapIndex: %v", err)
	}
	if err := m.WaitBackground(context.Background()); err != nil {
		t.Fatalf("WaitBackground: %v", err)
	}
	manifest, err := m.loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manifest.Originals["img/photo.jpg"]; !ok {
		t.Fatalf("inventory not persisted: %+v", manifest.Originals)
	}
}
