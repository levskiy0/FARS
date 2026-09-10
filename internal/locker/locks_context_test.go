package locker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLockContextCancellationPreservesOwnerAndCleansWaiter(t *testing.T) {
	locks := New()
	release := locks.Lock("key")
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := locks.LockContext(ctx, "key"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	locks.mu.Lock()
	refs := locks.locks["key"].refs
	locks.mu.Unlock()
	if refs != 1 {
		t.Fatalf("cancelled waiter leaked: refs=%d", refs)
	}
	release()
	next, err := locks.LockContext(context.Background(), "key")
	if err != nil {
		t.Fatal(err)
	}
	next()
	if len(locks.locks) != 0 {
		t.Fatal("lock entry leaked")
	}
}
