package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const defaultMonitorWorkers = 4
const defaultInvalidationLockTimeout = 100 * time.Millisecond

// bootstrapState belongs to the discovery goroutine. Failed paths alone are retried.
type bootstrapState struct {
	previous    originalManifest
	directories map[string]struct{}
	references  map[string][]string
}

func (m *Manager) monitorWorkers() int {
	if m.cfg.Cache.CheckOriginalsWorkers > 0 {
		return m.cfg.Cache.CheckOriginalsWorkers
	}
	return defaultMonitorWorkers
}

func (m *Manager) invalidationTimeout() time.Duration {
	if m.cfg.Cache.InvalidationLockTimeout.Duration > 0 {
		return m.cfg.Cache.InvalidationLockTimeout.Duration
	}
	return defaultInvalidationLockTimeout
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// StartOriginalMonitor starts independent discovery, checking, deletion and persistence loops.
// Workers are long-lived. A stuck syscall occupies one worker, not a new goroutine per tick.
func (m *Manager) StartOriginalMonitor(ctx context.Context) {
	interval := m.cfg.Cache.CheckOriginalsInterval.Duration
	if interval <= 0 {
		m.logger.Info("original monitor disabled")
		return
	}
	start := func(run func()) {
		m.background.Add(1)
		go func() { defer m.background.Done(); run() }()
	}
	start(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if err := m.bootstrapIndex(ctx); err != nil && ctx.Err() == nil {
				m.logger.Error("cache discovery incomplete; known originals remain monitored", slog.Any("error", err))
			}
			signal(m.checkWake)
			signal(m.manifestWake)
			m.indexMu.RLock()
			ready := m.indexReady
			m.indexMu.RUnlock()
			if ready {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
	start(func() {
		m.runKeyedWorkers(ctx, interval, m.checkWake, "original checks", m.originalKeys, m.checkOriginal)
	})
	// Invalidation is independent of slow metadata checks and does not wait another full interval.
	retryInterval := min(interval, time.Second)
	start(func() {
		m.runKeyedWorkers(ctx, retryInterval, nil, "cache invalidations", m.pendingKeys, m.invalidatePending)
	})
	start(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-m.manifestWake:
			}
			if err := m.flushManifest(); err != nil {
				m.logger.Error("save original manifest; will retry", slog.Any("error", err))
			}
		}
	})
}

type workerResult struct {
	key string
	err error
}

// Dispatch continues across ticks without waiting for a whole batch to finish.
// In-flight keys are excluded from later batches, including permanently blocked calls.
func (m *Manager) runKeyedWorkers(
	ctx context.Context, interval time.Duration, wake <-chan struct{}, name string,
	snapshot func() []string, work func(context.Context, string) error,
) {
	jobs := make(chan string)
	results := make(chan workerResult, m.monitorWorkers())
	var workers sync.WaitGroup
	for range m.monitorWorkers() {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case key, ok := <-jobs:
					if !ok {
						return
					}
					err := work(ctx, key)
					select {
					case results <- workerResult{key, err}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	defer func() { close(jobs); workers.Wait() }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	inFlight := make(map[string]time.Time)
	keys := snapshot()
	cursor := 0
	completed, failed := 0, 0
	for {
		for cursor < len(keys) {
			if _, busy := inFlight[keys[cursor]]; !busy {
				break
			}
			cursor++
		}
		var output chan string
		var key string
		if cursor < len(keys) {
			output = jobs
			key = keys[cursor]
		}
		select {
		case <-ctx.Done():
			return
		case output <- key:
			inFlight[key] = time.Now()
			cursor++
		case result := <-results:
			delete(inFlight, result.key)
			completed++
			if result.err != nil && ctx.Err() == nil {
				failed++
				m.logger.Warn("cache worker task deferred", slog.String("job", name), slog.String("path", result.key), slog.Any("error", result.err))
			}
		case <-ticker.C:
			oldest := time.Duration(0)
			for _, since := range inFlight {
				oldest = max(oldest, time.Since(since))
			}
			if completed > 0 || failed > 0 || len(inFlight) > 0 || name == "original checks" {
				m.logger.Info("cache monitor progress", slog.String("job", name),
					slog.Int("completed", completed), slog.Int("failed", failed),
					slog.Int("in_flight", len(inFlight)), slog.Int("not_dispatched", len(keys)-cursor), slog.Duration("oldest_in_flight", oldest))
			}
			completed, failed = 0, 0
			// Finish dispatching the current snapshot before refreshing, to avoid starvation.
			if cursor == len(keys) {
				keys = snapshot()
				cursor = 0
			}
		case <-wake:
			if cursor == len(keys) {
				keys = snapshot()
				cursor = 0
			}
		}
	}
}

func (m *Manager) originalKeys() []string {
	m.indexMu.RLock()
	keys := make([]string, 0, len(m.originals))
	for key := range m.originals {
		keys = append(keys, key)
	}
	m.indexMu.RUnlock()
	sort.Strings(keys)
	return keys
}

func (m *Manager) pendingKeys() []string {
	m.indexMu.RLock()
	keys := make([]string, 0, len(m.pendingOriginals)+len(m.pendingOrphans))
	for key := range m.pendingOriginals {
		keys = append(keys, "s:"+key)
	}
	for key := range m.pendingOrphans {
		keys = append(keys, "o:"+key)
	}
	m.indexMu.RUnlock()
	sort.Strings(keys)
	return keys
}

func (m *Manager) markPendingLocked(rel string, entry *trackedOriginal) {
	if !entry.Pending {
		entry.Pending = true
		m.indexGeneration++
	}
	m.pendingOriginals[rel] = struct{}{}
}

func (m *Manager) checkOriginal(ctx context.Context, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.indexMu.RLock()
	entry := m.originals[rel]
	if entry == nil || entry.Pending {
		m.indexMu.RUnlock()
		return nil
	}
	signature := entry.Signature
	m.indexMu.RUnlock()
	info, err := m.sourceStat(filepath.Join(m.cfg.Storage.BaseDir, filepath.FromSlash(rel)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat original %q: %w", rel, err)
	}
	if err == nil && info.Mode().IsRegular() && signatureFromInfo(info) == signature {
		return nil
	}
	m.indexMu.Lock()
	// Do not invalidate a replacement index entry based on an obsolete observation.
	if current := m.originals[rel]; current != nil && current.Signature == signature {
		m.markPendingLocked(rel, current)
	}
	m.indexMu.Unlock()
	return nil
}

func (m *Manager) invalidatePending(ctx context.Context, key string) error {
	// Bounds lock waiting and time between removal operations. It cannot interrupt a syscall.
	taskCtx, cancel := context.WithTimeout(ctx, m.invalidationTimeout())
	defer cancel()
	if key[:2] == "s:" {
		_, err := m.invalidateOriginal(taskCtx, key[2:], false)
		return err
	}
	path := key[2:]
	release, err := m.locks.LockContext(taskCtx, "cache:"+filepath.Clean(path))
	if err != nil {
		return err
	}
	defer release()
	// Discovery may have observed a missing source before a request restored and
	// regenerated this path. Recheck ownership under the publication lock so an
	// old orphan task cannot delete a newly registered resize.
	m.indexMu.Lock()
	_, pending := m.pendingOrphans[path]
	_, registered := m.variantToOriginal[filepath.Clean(path)]
	if registered {
		delete(m.pendingOrphans, path)
	}
	m.indexMu.Unlock()
	if !pending || registered {
		return nil
	}
	_, err = m.removeCacheFileLocked(path, nil)
	if err == nil {
		m.indexMu.Lock()
		delete(m.pendingOrphans, path)
		m.indexMu.Unlock()
	}
	return err
}

// checkOriginalsOnce is a finite pass used by diagnostics/tests. The service uses
// persistent workers above, so a slow item cannot prevent later ticks for other keys.
func (m *Manager) checkOriginalsOnce(ctx context.Context) error {
	checkErr := m.parallelTasks(ctx, m.originalKeys(), func(key string) error { return m.checkOriginal(ctx, key) })
	return errors.Join(checkErr, m.drainPendingOnce(ctx))
}

func (m *Manager) drainPendingOnce(ctx context.Context) error {
	return m.parallelTasks(ctx, m.pendingKeys(), func(key string) error { return m.invalidatePending(ctx, key) })
}

func (m *Manager) parallelTasks(ctx context.Context, keys []string, work func(string) error) error {
	jobs := make(chan string)
	var workers sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	failed := 0
	for range min(m.monitorWorkers(), len(keys)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for key := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if err := work(key); err != nil {
					mu.Lock()
					failed++
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, key := range keys {
		select {
		case jobs <- key:
		case <-ctx.Done():
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if firstErr != nil {
		return fmt.Errorf("%d cache tasks failed; first: %w", failed, firstErr)
	}
	return nil
}

// bootstrapIndex only discovers dependencies and queues invalidations.
// Neither successful deletion nor manifest persistence is a prerequisite for checking.
func (m *Manager) bootstrapIndex(ctx context.Context) error {
	m.bootstrapMu.Lock()
	defer m.bootstrapMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.bootstrapState == nil {
		previous, err := m.loadManifest()
		if err != nil {
			m.logger.Warn("load original manifest", slog.Any("error", err))
			previous = originalManifest{Originals: make(map[string]fileSignature)}
		}
		m.bootstrapState = &bootstrapState{
			previous:    previous,
			directories: map[string]struct{}{m.cfg.Storage.CacheDir: {}},
			references:  make(map[string][]string),
		}
		m.indexMu.Lock()
		m.previousManifest = previous
		m.manifestLoaded = true
		m.indexMu.Unlock()
	}
	state := m.bootstrapState
	if len(state.directories) == 0 && len(state.references) == 0 {
		return nil
	}
	started := time.Now()
	directories := make([]string, 0, len(state.directories))
	for path := range state.directories {
		directories = append(directories, path)
	}
	sort.Strings(directories)
	var firstErr error
	failedDirs := 0
	for _, root := range directories {
		delete(state.directories, root)
		walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				state.directories[path] = struct{}{}
				failedDirs++
				if firstErr == nil {
					firstErr = err
				}
				return nil // WalkDir continues with accessible siblings.
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isAllowedCacheExt(path) {
				return nil
			}
			_, rel, ok := splitCachePath(m.cfg.Storage.CacheDir, path)
			if !ok {
				return nil
			}
			rel = filepath.ToSlash(filepath.Clean(rel))
			for _, existing := range state.references[rel] {
				if existing == path {
					return nil
				}
			}
			state.references[rel] = append(state.references[rel], path)
			return nil
		})
		if walkErr != nil {
			state.directories[root] = struct{}{}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if firstErr == nil {
				firstErr = walkErr
			}
		}
	}
	references := make([]string, 0, len(state.references))
	for rel := range state.references {
		references = append(references, rel)
	}
	sort.Strings(references)
	// Keep failures for the next discovery attempt; no healthy path is rescanned.
	var doneMu sync.Mutex
	var done []string
	referenceErr := m.parallelTasks(ctx, references, func(rel string) error {
		if err := m.indexCacheReference(ctx, rel, state.references[rel], state.previous); err != nil {
			return err
		}
		doneMu.Lock()
		done = append(done, rel)
		doneMu.Unlock()
		return nil
	})
	for _, rel := range done {
		delete(state.references, rel)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.indexMu.Lock()
	ready := len(state.directories) == 0 && len(state.references) == 0
	if ready != m.indexReady {
		m.indexReady = ready
		if ready {
			// No unresolved cache references remain; active entries now own all
			// remaining invalidations and retired history can be released.
			m.bootstrapRetired = nil
		}
		m.indexGeneration++
	}
	m.indexMu.Unlock()
	stats := m.Stats()
	m.logger.Info("cache discovery progress", slog.Bool("complete", ready),
		slog.Int("originals", stats.Originals), slog.Int("variants", stats.Variants),
		slog.Int("retry_directories", len(state.directories)), slog.Int("retry_references", len(state.references)),
		slog.Duration("duration", time.Since(started)))
	if firstErr != nil {
		firstErr = fmt.Errorf("%d directory errors; first: %w", failedDirs, firstErr)
	}
	return errors.Join(firstErr, referenceErr)
}

func (m *Manager) indexCacheReference(ctx context.Context, cacheRel string, paths []string, previous originalManifest) error {
	originalRel, info, err := m.resolveOriginalInfo(cacheRel)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve %q: %w", cacheRel, err)
		}
		m.indexMu.Lock()
		for _, path := range paths {
			m.pendingOrphans[path] = struct{}{}
		}
		m.indexMu.Unlock()
		return nil
	}
	lockCtx, cancel := context.WithTimeout(ctx, m.invalidationTimeout())
	defer cancel()
	release, err := m.locks.LockContext(lockCtx, "original:"+originalRel)
	if err != nil {
		return err
	}
	defer release()
	stale := previous.Pending[originalRel]
	if old, ok := previous.Originals[originalRel]; ok && old != signatureFromInfo(info) {
		stale = true
	}
	for _, path := range paths {
		if err := lockCtx.Err(); err != nil {
			return err
		}
		unlock, err := m.locks.LockContext(lockCtx, "cache:"+filepath.Clean(path))
		if err != nil {
			return err
		}
		cacheInfo, statErr := os.Lstat(path)
		if statErr == nil && cacheInfo.Mode().IsRegular() {
			m.trackVariant(originalRel, info, path)
			if stale || info.ModTime().After(cacheInfo.ModTime()) {
				m.indexMu.Lock()
				if entry := m.originals[originalRel]; entry != nil {
					m.markPendingLocked(originalRel, entry)
				}
				m.indexMu.Unlock()
			}
		}
		unlock()
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
	}
	return nil
}
