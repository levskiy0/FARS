package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriteRefusesEmptyPayload(t *testing.T) {
	m, base, cache := newTestManager(t)
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("original"))
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cache, "200x200", filepath.FromSlash(rel))
	if err := m.Write(path, rel, info, nil); err == nil {
		t.Fatal("cached a nil payload")
	}
	if err := m.Write(path, rel, info, []byte{}); err == nil {
		t.Fatal("cached an empty payload")
	}
	assertNotExists(t, path)
	if got := m.Stats(); got != (IndexStats{}) {
		t.Fatalf("empty payload registered in the index: %+v", got)
	}
}

func TestZeroLengthCacheFileIsAMiss(t *testing.T) {
	m, base, cache := newTestManager(t)
	rel := "img/photo.jpg"
	source := writeTestFile(t, base, rel, []byte("original"))
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	// What an interrupted publication leaves behind.
	truncated := writeTestFile(t, cache, "200x200/"+rel, nil)
	setTestMtime(t, truncated, time.Now())

	if m.IsFresh(truncated, info) {
		t.Fatal("a zero-length cache file was treated as a hit")
	}
	if m.IsFresh(truncated, nil) {
		t.Fatal("a zero-length cache file was treated as a hit without source metadata")
	}
	stats, file, err := m.ServeFileStats(truncated)
	if err == nil {
		file.Close()
		t.Fatalf("served a zero-length cache file: %+v", stats)
	}
	if !errors.Is(err, errEmptyCacheFile) {
		t.Fatalf("unexpected error for a zero-length cache file: %v", err)
	}
	if file != nil {
		t.Fatal("ServeFileStats leaked an open file handle on rejection")
	}

	// A real payload at the same path is served normally.
	if err := os.WriteFile(truncated, []byte("resize"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, file, err := m.ServeFileStats(truncated); err != nil {
		t.Fatalf("rejected a valid cache file: %v", err)
	} else {
		file.Close()
	}
}

func TestCleanupRemovesOnlyStaleTempFiles(t *testing.T) {
	m, base, cache := newTestManager(t)
	writeTestFile(t, base, "img/photo.jpg", []byte("original"))
	abandoned := writeTestFile(t, cache, "200x200/img/photo.jpg.123456"+tempFileSuffix, []byte("half written"))
	inFlight := writeTestFile(t, cache, "200x200/img/photo.jpg.789012"+tempFileSuffix, []byte("still writing"))
	setTestMtime(t, abandoned, time.Now().Add(-2*staleTempFileAge))
	setTestMtime(t, inFlight, time.Now())

	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	assertNotExists(t, abandoned)
	if data, err := os.ReadFile(inFlight); err != nil || string(data) != "still writing" {
		t.Fatalf("cleanup destroyed a publication in progress: %q, %v", data, err)
	}
}

// TestPublicationTempFileIsUnique pins the per-publication temp name: a second
// writer (another process on the same cache volume, or a concurrent request)
// must not be able to truncate or steal the temp file of the first.
func TestPublicationTempFileIsUnique(t *testing.T) {
	cache := t.TempDir()
	path := filepath.Join(cache, "200x200", "img", "photo.jpg")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// The name a fixed-name implementation would reuse, standing in for the
	// temp file of a concurrent publication.
	decoy := path + tempFileSuffix
	if err := os.WriteFile(decoy, []byte("another publication in progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("resize")); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "resize" {
		t.Fatalf("publication did not land: %q, %v", data, err)
	}
	if data, err := os.ReadFile(decoy); err != nil || string(data) != "another publication in progress" {
		t.Fatalf("publication clobbered a concurrent writer's temp file: %q, %v", data, err)
	}
}

func TestConcurrentPublicationsToSamePathDoNotClobber(t *testing.T) {
	cache := t.TempDir()
	path := filepath.Join(cache, "200x200", "img", "photo.jpg")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const writers = 8
	payloads := make(map[string]struct{}, writers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		payload := fmt.Sprintf("resize-%02d-%s", i, strings.Repeat("x", 4096))
		payloads[payload] = struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := writeFileAtomic(path, []byte(payload)); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent publication failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := payloads[string(data)]; !ok {
		t.Fatalf("published file is not any single payload (len %d)", len(data))
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), tempFileSuffix) {
			t.Fatalf("temp file left behind: %s", entry.Name())
		}
	}
}
