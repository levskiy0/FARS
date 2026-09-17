package locker

import (
	"context"
	"testing"
	"time"
)

// waitOrFail runs work in the background and fails the test instead of hanging
// the suite when it deadlocks.
func waitOrFail(t *testing.T, what string, work func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		work()
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	select {
	case <-done:
	case <-deadline.C:
		t.Fatalf("%s deadlocked", what)
	}
}

// TestDoubleReleaseIsANoOp covers a release closure handed to more than one
// owner (a defer plus an explicit call): the second release must not refill the
// token channel and wedge the key.
func TestDoubleReleaseIsANoOp(t *testing.T) {
	locks := New()
	release := locks.Lock("image")
	waitOrFail(t, "double release", func() {
		release()
		release()
		release()
	})
	locks.mu.Lock()
	entries := len(locks.locks)
	locks.mu.Unlock()
	if entries != 0 {
		t.Fatalf("double release leaked %d map entries", entries)
	}

	var second func()
	waitOrFail(t, "re-lock after double release", func() {
		release, err := locks.LockContext(context.Background(), "image")
		if err != nil {
			t.Errorf("re-lock: %v", err)
			return
		}
		second = release
	})
	if second == nil {
		t.Fatal("key could not be re-locked")
	}
	second()
	locks.mu.Lock()
	entries = len(locks.locks)
	locks.mu.Unlock()
	if entries != 0 {
		t.Fatalf("lock entry leaked after re-lock: %d", entries)
	}
}

// TestDoubleReleaseDoesNotHandTheKeyToTwoOwners is the consequence that matters:
// an over-released key must not let a second holder in while the first is still
// working.
func TestDoubleReleaseDoesNotHandTheKeyToTwoOwners(t *testing.T) {
	locks := New()
	first := locks.Lock("image")
	first()
	first()

	second := locks.Lock("image")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if release, err := locks.LockContext(ctx, "image"); err == nil {
		release()
		second()
		t.Fatal("two owners held the same key at once")
	}
	second()
}
