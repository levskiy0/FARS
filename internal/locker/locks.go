package locker

import "sync"

// KeyedLocker provides fine-grained locks per cache key to avoid duplicate work.
type KeyedLocker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New creates a new keyed locker.
func New() *KeyedLocker {
	return &KeyedLocker{locks: make(map[string]*sync.Mutex)}
}

// Lock acquires the lock for the provided key.
func (k *KeyedLocker) Lock(key string) func() {
	mutex := k.get(key)
	mutex.Lock()
	return func() {
		mutex.Unlock()
		k.release(key, mutex)
	}
}

func (k *KeyedLocker) get(key string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if m, ok := k.locks[key]; ok {
		return m
	}
	m := &sync.Mutex{}
	k.locks[key] = m
	return m
}

func (k *KeyedLocker) release(key string, mutex *sync.Mutex) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if current, ok := k.locks[key]; ok && current == mutex {
		delete(k.locks, key)
	}
}
