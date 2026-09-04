package cache

import (
	"context"
	"os"
	"testing"
)

func TestVariantReassignedFromFallbackToExactSource(t *testing.T) {
	m, base, cache := newTestManager(t)
	oldSource := writeTestFile(t, base, "img/photo.jpg", []byte("old source"))
	path := writeTestFile(t, cache, "200x200/img/photo.jpg.webp", []byte("old resize"))
	if err := bootstrapForTest(m); err != nil {
		t.Fatal(err)
	}
	exact := writeTestFile(t, base, "img/photo.jpg.webp", []byte("independent exact source"))
	info, err := os.Stat(exact)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Write(path, "img/photo.jpg.webp", info, []byte("new resize")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldSource, []byte("old fallback changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.checkOriginalsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Stats(); got != (IndexStats{Originals: 1, Variants: 1}) {
		t.Fatalf("bad ownership: %+v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("independent variant removed: %v", err)
	}
}
