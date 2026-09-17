package locker

import (
	"context"
	"sync"
)

// KeyedLocker provides fine-grained locks per cache key to avoid duplicate work.
type KeyedLocker struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	token chan struct{}
	refs  int
}

// New creates a new keyed locker.
func New() *KeyedLocker {
	return &KeyedLocker{locks: make(map[string]*entry)}
}

// Lock acquires the lock for the provided key.
func (k *KeyedLocker) Lock(key string) func() {
	release, _ := k.LockContext(context.Background(), key)
	return release
}

// LockContext reserves the same keyed lock as Lock, with cancellable waiting.
func (k *KeyedLocker) LockContext(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	item := k.get(key)
	select {
	case <-ctx.Done():
		k.release(key, item)
		return nil, ctx.Err()
	case <-item.token:
		if err := ctx.Err(); err != nil {
			item.token <- struct{}{}
			k.release(key, item)
			return nil, err
		}
		// A release closure may be handed to several owners (defer plus an explicit
		// call); releasing twice would refill the token channel and wedge the key.
		var once sync.Once
		return func() {
			once.Do(func() {
				item.token <- struct{}{}
				k.release(key, item)
			})
		}, nil
	}
}

func (k *KeyedLocker) get(key string) *entry {
	k.mu.Lock()
	defer k.mu.Unlock()
	if m, ok := k.locks[key]; ok {
		m.refs++
		return m
	}
	m := &entry{refs: 1, token: make(chan struct{}, 1)}
	m.token <- struct{}{}
	k.locks[key] = m
	return m
}

func (k *KeyedLocker) release(key string, mutex *entry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	mutex.refs--
	if mutex.refs == 0 {
		delete(k.locks, key)
	}
}
