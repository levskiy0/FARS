package locker

import (
	"sync"
	"testing"
)

func TestWaiterKeepsSameLockReserved(t *testing.T) {
	locks := New()
	owner := locks.get("image")
	waiter := locks.get("image")
	locks.release("image", owner)
	newcomer := locks.get("image")
	if waiter != newcomer {
		t.Fatal("newcomer bypassed the queued waiter with a different mutex")
	}
	locks.release("image", waiter)
	locks.release("image", newcomer)
	if len(locks.locks) != 0 {
		t.Fatal("unused lock leaked")
	}
}
func TestConcurrentSameKey(t *testing.T) {
	locks := New()
	count := 0
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				release := locks.Lock("same")
				count++
				release()
			}
		}()
	}
	wg.Wait()
	if count != 2000 {
		t.Fatalf("lost updates: %d", count)
	}
}
