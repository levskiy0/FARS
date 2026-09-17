package cache

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedSizedCache lays down three equally sized variants, each in its own
// eviction bucket, with originals old enough not to invalidate them.
func seedSizedCache(t *testing.T, base, cache string) (paths []string, each int64) {
	t.Helper()
	payload := bytes.Repeat([]byte("x"), 1024)
	anchor := time.Unix(1_700_000_000, 0)
	for i, name := range []string{"oldest", "middle", "newest"} {
		source := writeTestFile(t, base, "img/"+name+".jpg", []byte("original"))
		setTestMtime(t, source, anchor.Add(-time.Hour))
		variant := writeTestFile(t, cache, "200x200/img/"+name+".jpg", payload)
		setTestMtime(t, variant, anchor.Add(time.Duration(i)*2*evictionBucket))
		paths = append(paths, variant)
	}
	return paths, int64(len(payload))
}

func TestSizeCapEvictsOldestFirst(t *testing.T) {
	m, base, cache := newTestManager(t)
	paths, each := seedSizedCache(t, base, cache)
	// Room for two of the three entries.
	m.SetMaxCacheSize(2*each + each/2)

	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	assertNotExists(t, paths[0])
	for _, path := range paths[1:] {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("evicted more than needed, %s is gone: %v", path, err)
		}
	}
	if total := cacheBytes(t, cache); total > m.maxCacheSize.Load() {
		t.Fatalf("cache still over the cap after eviction: %d > %d", total, m.maxCacheSize.Load())
	}
}

func TestSizeCapEvictsUntilUnderTheLimit(t *testing.T) {
	m, base, cache := newTestManager(t)
	paths, each := seedSizedCache(t, base, cache)
	// Room for one entry only: the two oldest have to go.
	m.SetMaxCacheSize(each + each/2)

	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	assertNotExists(t, paths[0])
	assertNotExists(t, paths[1])
	if _, err := os.Stat(paths[2]); err != nil {
		t.Fatalf("newest entry evicted: %v", err)
	}
}

func TestZeroSizeCapDisablesEviction(t *testing.T) {
	m, base, cache := newTestManager(t)
	paths, _ := seedSizedCache(t, base, cache)
	m.SetMaxCacheSize(0)

	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("evicted %s with size capping disabled: %v", path, err)
		}
	}
}

func TestNegativeSizeCapIsTreatedAsUnlimited(t *testing.T) {
	m, base, cache := newTestManager(t)
	paths, _ := seedSizedCache(t, base, cache)
	m.SetMaxCacheSize(-1)
	if got := m.maxCacheSize.Load(); got != 0 {
		t.Fatalf("negative cap stored as %d", got)
	}
	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("evicted %s with a negative cap: %v", path, err)
		}
	}
}

func cacheBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && isAllowedCacheExt(path) {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// TestSizeCapEvictsLeastRecentlyUsed is the whole point of the cap: a cache file
// is never rewritten after publication, so without a refresh on hit the oldest
// file is the one published first - typically the catalogue image every page
// loads - and eviction turns into a re-render loop.
func TestSizeCapEvictsLeastRecentlyUsed(t *testing.T) {
	m, base, cache := newTestManager(t)
	paths, each := seedSizedCache(t, base, cache)
	// Room for two of the three entries.
	m.SetMaxCacheSize(2*each + each/2)

	// The oldest entry is the hot one: serving it makes it the most recent.
	if !m.IsFresh(paths[0], nil) {
		t.Fatal("hot entry reported as stale")
	}
	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("the entry that was just served was evicted first: %v", err)
	}
	assertNotExists(t, paths[1])
	if _, err := os.Stat(paths[2]); err != nil {
		t.Fatalf("evicted more than needed: %v", err)
	}
}

func TestCacheHitRefreshIsRateLimited(t *testing.T) {
	m, base, cache := newTestManager(t)
	paths, _ := seedSizedCache(t, base, cache)
	m.SetMaxCacheSize(1 << 20)
	recent := time.Now().Add(-cacheHitTouchInterval / 2)
	setTestMtime(t, paths[0], recent)

	if !m.IsFresh(paths[0], nil) {
		t.Fatal("entry reported as stale")
	}
	info, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(recent) {
		t.Fatalf("a recently refreshed entry was touched again: %v", info.ModTime())
	}

	old := time.Now().Add(-2 * cacheHitTouchInterval)
	setTestMtime(t, paths[0], old)
	if !m.IsFresh(paths[0], nil) {
		t.Fatal("entry reported as stale")
	}
	info, err = os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(old) {
		t.Fatal("a hit on an entry older than the refresh interval did not refresh it")
	}
}

// TestCacheHitDoesNotRefreshWithoutASizeCap keeps TTL semantics unchanged for
// every deployment that runs without a cap: there, an entry expires a fixed time
// after it was published, not after it was last served.
func TestCacheHitDoesNotRefreshWithoutASizeCap(t *testing.T) {
	m, base, cache := newTestManager(t)
	paths, _ := seedSizedCache(t, base, cache)
	m.SetMaxCacheSize(0)
	m.cfg.Cache.TTL.Duration = 30 * 24 * time.Hour
	old := time.Now().Add(-2 * cacheHitTouchInterval)
	setTestMtime(t, paths[0], old)

	if !m.IsFresh(paths[0], nil) {
		t.Fatal("entry reported as stale")
	}
	info, err := os.Stat(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("cache entry touched although no size cap is configured: %v", info.ModTime())
	}
}
