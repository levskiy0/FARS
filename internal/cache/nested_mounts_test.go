package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// nestedMountLayout reproduces the production bind-mount shape: base_dir itself
// is an ordinary directory and img/, modules/ and themes/ are separate mounts
// Docker creates whether or not anything is mounted into them.
func nestedMountLayout(t *testing.T, base string) {
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
	for _, name := range []string{"img", "modules", "themes"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCleanupSkipsOrphanRemovalWithEmptyNestedMounts is the incident itself: the
// three nested mount points exist and are empty, so "base_dir has a dirent" is
// true while the volume holds no originals at all.
func TestCleanupSkipsOrphanRemovalWithEmptyNestedMounts(t *testing.T) {
	m, base, cache := newTestManager(t)
	m.cfg.Cache.TTL.Duration = 30 * 24 * time.Hour
	variants := []string{
		writeTestFile(t, cache, "200x200/img/p/1/1/11.jpg", []byte("resize")),
		writeTestFile(t, cache, "400x400/modules/mod/views/img/logo.png", []byte("resize")),
		writeTestFile(t, cache, "100x100/themes/theme/assets/x.jpg", []byte("resize")),
		// An original that sits directly in base_dir: only the root probe can
		// tell whether the volume behind it is there.
		writeTestFile(t, cache, "200x200/loose.jpg", []byte("resize")),
	}
	for _, variant := range variants {
		setTestMtime(t, variant, time.Now())
	}
	nestedMountLayout(t, base)

	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	for _, variant := range variants {
		if _, err := os.Stat(variant); err != nil {
			t.Fatalf("cache wiped while the nested originals mounts were empty: %s: %v", variant, err)
		}
	}
}

// TestOriginalsProbeFindsDeeplyNestedOriginals is the other half: a volume that
// really is populated must not be mistaken for an unmounted one, even though the
// first regular file sits several levels below base_dir.
func TestOriginalsProbeFindsDeeplyNestedOriginals(t *testing.T) {
	m, base, cache := newTestManager(t)
	m.cfg.Cache.TTL.Duration = 30 * 24 * time.Hour
	nestedMountLayout(t, base)
	writeTestFile(t, base, "img/p/1/1/11.jpg", []byte("original"))
	if !m.originalsRootPopulated() {
		t.Fatal("a populated originals volume was reported as unavailable")
	}
	orphan := writeTestFile(t, cache, "200x200/img/p/2/2/22.jpg", []byte("resize"))
	setTestMtime(t, orphan, time.Now())
	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	assertNotExists(t, orphan)
}

// TestOriginalsProbeTreatsUnreadableRootAsUnavailable keeps the probe fail-safe:
// an inconclusive answer must never authorise deletion.
func TestOriginalsProbeTreatsUnreadableRootAsUnavailable(t *testing.T) {
	m, base, _ := newTestManager(t)
	nestedMountLayout(t, base)
	blocked := filepath.Join(base, "img")
	writeTestFile(t, base, "img/photo.jpg", []byte("original"))
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	if _, err := os.ReadDir(blocked); err == nil {
		t.Skip("filesystem/user bypasses directory permission checks")
	}
	if m.originalsRootPopulated() {
		t.Fatal("an unreadable originals volume was reported as populated")
	}
}

// TestOrphanRemovalStopsWhenTheVolumeDisappearsMidPass covers the second half of
// the guard: a pass over a large cache takes minutes, and the decision made at
// the top of it is worthless once the volume goes away in the middle.
func TestOrphanRemovalStopsWhenTheVolumeDisappearsMidPass(t *testing.T) {
	m, base, cache := newTestManager(t)
	m.cfg.Cache.TTL.Duration = 30 * 24 * time.Hour
	writeTestFile(t, base, "img/present.jpg", []byte("original"))
	first := writeTestFile(t, cache, "200x200/img/a-gone.jpg", []byte("resize"))
	second := writeTestFile(t, cache, "200x200/img/z-gone.jpg", []byte("resize"))
	for _, variant := range []string{first, second} {
		setTestMtime(t, variant, time.Now())
	}
	if !m.originalsRootPopulatedCached() {
		t.Fatal("populated volume reported as unavailable before the pass")
	}

	// The volume is pulled out while the pass is resolving its first orphan.
	m.sourceStat = func(path string) (os.FileInfo, error) {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			emptyOriginalsRoot(t, base, false)
			resetRootProbe(m)
		}
		return info, err
	}

	if err := m.cleanupOnce(context.Background()); err != nil {
		t.Fatalf("cleanupOnce: %v", err)
	}
	for _, variant := range []string{first, second} {
		if _, err := os.Stat(variant); err != nil {
			t.Fatalf("orphan deleted after the originals volume vanished mid-pass: %s: %v", variant, err)
		}
	}
}
