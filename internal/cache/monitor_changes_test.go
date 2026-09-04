package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanInvalidatesOnlyChangedOriginal(t *testing.T) {
	for _, change := range []string{"size", "mtime forward", "mtime backward", "same size and mtime", "deleted", "replaced by directory", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			m, base, cacheDir := newTestManager(t)
			rel := "img/p/1/2/12.jpg"
			source := writeTestFile(t, base, rel, []byte("before"))
			mtime := time.Unix(1700000000, 0)
			setTestMtime(t, source, mtime)
			before, err := os.Stat(source)
			if err != nil {
				t.Fatal(err)
			}
			var variants []string
			for _, cached := range []string{"200x200/" + rel, "400x400/" + rel + ".webp", "0x100/" + rel + ".avif"} {
				variants = append(variants, writeTestFile(t, cacheDir, cached, []byte("resize")))
			}
			writeTestFile(t, base, "img/other.jpg", []byte("other"))
			other := writeTestFile(t, cacheDir, "200x200/img/other.jpg", []byte("keep"))
			if err := bootstrapForTest(m); err != nil {
				t.Fatal(err)
			}

			switch change {
			case "size":
				if err := os.WriteFile(source, []byte("longer original"), 0644); err != nil {
					t.Fatal(err)
				}
				setTestMtime(t, source, mtime)
			case "mtime forward":
				setTestMtime(t, source, mtime.Add(time.Hour))
			case "mtime backward":
				setTestMtime(t, source, mtime.Add(-time.Hour))
			case "same size and mtime":
				if err := os.WriteFile(source, []byte("after!"), 0644); err != nil {
					t.Fatal(err)
				}
				setTestMtime(t, source, mtime)
				after, err := os.Stat(source)
				if err != nil {
					t.Fatal(err)
				}
				if signatureFromInfo(before) == signatureFromInfo(after) {
					t.Skip("filesystem/platform does not expose changed ctime for this write")
				}
			case "deleted", "replaced by directory":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if change == "replaced by directory" {
					if err := os.Mkdir(source, 0755); err != nil {
						t.Fatal(err)
					}
				}
			}

			if err := m.checkOriginalsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, path := range variants {
				if change == "unchanged" {
					if data, err := os.ReadFile(path); err != nil || string(data) != "resize" {
						t.Errorf("unchanged resize lost: %q, %v", data, err)
					}
				} else {
					assertNotExists(t, path)
				}
			}
			if data, err := os.ReadFile(other); err != nil || string(data) != "keep" {
				t.Fatalf("unrelated resize changed: %q, %v", data, err)
			}
			if change != "unchanged" {
				// An already drained queue must not keep deleting newly generated variants.
				if change == "replaced by directory" {
					if err := os.Remove(source); err != nil {
						t.Fatal(err)
					}
				}
				writeTestFile(t, base, rel, []byte("latest"))
				info, err := os.Stat(source)
				if err != nil {
					t.Fatal(err)
				}
				regenerated := filepath.Join(cacheDir, "200x200", rel)
				unlockSource := m.LockOriginal(rel)
				unlockCache := m.LockCache(regenerated)
				err = m.Write(regenerated, rel, info, []byte("fresh"))
				unlockCache()
				unlockSource()
				if err != nil {
					t.Fatal(err)
				}
				if err := m.checkOriginalsOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				if data, err := os.ReadFile(regenerated); err != nil || string(data) != "fresh" {
					t.Fatalf("regenerated resize lost: %q, %v", data, err)
				}
			}
		})
	}
}
