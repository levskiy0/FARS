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

// maxBootstrapPathFailures bounds how often one unreadable path is retried
// before discovery stops hammering it every interval and puts it on a back-off.
const maxBootstrapPathFailures = 10

// bootstrapRetryIntervals is the first back-off of a path discovery has given up
// on for now, counted in monitor intervals so that a deployment and a test scale
// together. It doubles on every further round of failures.
const bootstrapRetryIntervals = 10

// maxBootstrapRetryBackoff caps that doubling: an unreadable subtree is retried
// at least once an hour, because a permission problem is usually fixed from
// outside the process.
const maxBootstrapRetryBackoff = time.Hour

// errOriginalsUnavailable defers a reference whose original cannot be judged
// because the originals volume is missing or empty.
var errOriginalsUnavailable = errors.New("originals directory missing or empty")

// bootstrapState belongs to the discovery goroutine. Failed paths alone are retried.
type bootstrapState struct {
	previous    originalManifest
	directories map[string]struct{}
	references  map[string][]string
	failures    map[string]int
	// abandoned holds the paths that are waiting out a back-off. They stay in
	// directories/references, so they keep discovery incomplete: a path that was
	// dropped outright used to make discovery declare itself complete, which
	// cleared every Unknown marker, released the retired entries and stopped the
	// manifest from carrying the baselines of the subtree that was never read.
	// One directory owned by another uid would then permanently degrade the
	// index, with discovery gone for the lifetime of the process.
	abandoned map[string]retrySchedule
}

// retrySchedule is the back-off of one abandoned path.
type retrySchedule struct {
	at      time.Time
	backoff time.Duration
}

// fail records one more consecutive failure for a path and reports whether it
// should be given up on for now.
func (s *bootstrapState) fail(path string) bool {
	if s.failures == nil {
		s.failures = make(map[string]int)
	}
	s.failures[path]++
	return s.failures[path] >= maxBootstrapPathFailures
}

// abandon puts a repeatedly failing path on a back-off. The path itself is kept
// queued, so readiness keeps waiting for it; only the retry rate drops.
func (s *bootstrapState) abandon(path string, interval time.Duration) time.Duration {
	if s.abandoned == nil {
		s.abandoned = make(map[string]retrySchedule)
	}
	backoff := s.abandoned[path].backoff
	switch {
	case backoff <= 0:
		backoff = max(interval, time.Millisecond) * bootstrapRetryIntervals
	default:
		backoff *= 2
	}
	backoff = min(backoff, maxBootstrapRetryBackoff)
	s.abandoned[path] = retrySchedule{at: time.Now().Add(backoff), backoff: backoff}
	delete(s.failures, path)
	return backoff
}

// waiting reports whether path is still inside its back-off. A due path keeps
// its schedule until it succeeds, so the next failure doubles from where the
// previous one left off.
func (s *bootstrapState) waiting(path string, now time.Time) bool {
	schedule, ok := s.abandoned[path]
	return ok && now.Before(schedule.at)
}

// recovered clears the back-off history of a path that was read successfully.
func (s *bootstrapState) recovered(path string) {
	delete(s.failures, path)
	delete(s.abandoned, path)
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

// markPendingLocked condemns everything registered for an original right now.
// Variants published afterwards are produced from the current source and are not
// part of the set, which is what lets a hot original leave the queue at all.
// Caller holds indexMu.
func (m *Manager) markPendingLocked(rel string, entry *trackedOriginal) {
	for path := range entry.Variants {
		if _, ok := entry.Doomed[path]; ok {
			continue
		}
		if entry.Doomed == nil {
			entry.Doomed = make(map[string]struct{}, len(entry.Variants))
		}
		entry.Doomed[path] = struct{}{}
		m.indexGeneration++
	}
	// Variants discovery has not reached yet are stale too, and only this flag
	// can say so once they show up.
	if !m.indexReady && !entry.Unknown && m.cfg.Cache.CheckOriginalsInterval.Duration > 0 {
		entry.Unknown = true
		m.indexGeneration++
	}
	if len(entry.Doomed) > 0 {
		m.pendingOriginals[rel] = struct{}{}
	}
}

func (m *Manager) checkOriginal(ctx context.Context, rel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.indexMu.RLock()
	entry := m.originals[rel]
	if entry == nil || len(entry.Doomed) > 0 {
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
	// A source that disappeared along with its whole volume is an unmount, not a
	// deletion. Invalidating on that would empty the cache one original at a
	// time, which is the same loss the cleanup pass is guarded against.
	if errors.Is(err, os.ErrNotExist) && !m.originalsAvailableFor(rel) {
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
	if len(key) < 2 {
		return fmt.Errorf("malformed invalidation key %q", key)
	}
	if key[:2] == "s:" {
		_, err := m.invalidateOriginal(taskCtx, key[2:], nil)
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
dispatch:
	for _, key := range keys {
		select {
		case jobs <- key:
		case <-ctx.Done():
			break dispatch
		}
		if ctx.Err() != nil {
			break dispatch
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
			failures:    make(map[string]int),
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
	interval := m.cfg.Cache.CheckOriginalsInterval.Duration
	directories := make([]string, 0, len(state.directories))
	for path := range state.directories {
		if state.waiting(path, started) {
			continue
		}
		directories = append(directories, path)
	}
	sort.Strings(directories)
	var firstErr error
	failedDirs := 0
	// A path that no longer exists is not a failure: cleanup prunes empty cache
	// directories while discovery is walking them. Re-queueing those would keep
	// discovery incomplete for the lifetime of the process.
	deferPath := func(path string, err error) {
		if errors.Is(err, os.ErrNotExist) {
			delete(state.failures, path)
			return
		}
		failedDirs++
		if firstErr == nil {
			firstErr = err
		}
		if state.fail(path) {
			backoff := state.abandon(path, interval)
			m.logger.Warn("deferring cache directory after repeated failures; discovery stays incomplete",
				slog.String("path", path), slog.Int("attempts", maxBootstrapPathFailures),
				slog.Duration("retry_in", backoff), slog.Any("error", err))
		}
		state.directories[path] = struct{}{}
	}
	for _, root := range directories {
		delete(state.directories, root)
		deferred := false
		walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				deferred = deferred || path == root
				deferPath(path, err)
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
			if ctx.Err() != nil {
				state.directories[root] = struct{}{}
				return ctx.Err()
			}
			deferPath(root, walkErr)
			deferred = true
		}
		if !deferred {
			state.recovered(root)
		}
	}
	// An unmounted or empty originals volume makes every cached file look like an
	// orphan. Defer those references instead of queueing the whole cache for
	// deletion; discovery stays incomplete until the volume is back.
	m.forgetProbes()
	orphansAllowed := m.originalsRootPopulatedCached()
	if !orphansAllowed {
		m.logger.Warn("originals directory missing or empty; deferring orphan invalidation",
			slog.String("base_dir", m.cfg.Storage.BaseDir))
	}
	references := make([]string, 0, len(state.references))
	for rel := range state.references {
		if state.waiting(rel, started) {
			continue
		}
		references = append(references, rel)
	}
	sort.Strings(references)
	// Keep failures for the next discovery attempt; no healthy path is rescanned.
	var doneMu sync.Mutex
	var done []string
	referenceErr := m.parallelTasks(ctx, references, func(rel string) error {
		err := m.indexCacheReference(ctx, rel, state.references[rel], state.previous)
		doneMu.Lock()
		defer doneMu.Unlock()
		switch {
		case errors.Is(err, errOriginalsUnavailable):
			// Deferred, not failed: keep the reference for a later attempt.
			return nil
		case err != nil:
			if state.fail(rel) {
				backoff := state.abandon(rel, interval)
				m.logger.Warn("deferring cache reference after repeated failures; discovery stays incomplete",
					slog.String("path", rel), slog.Int("attempts", maxBootstrapPathFailures),
					slog.Duration("retry_in", backoff), slog.Any("error", err))
			}
			return err
		}
		state.recovered(rel)
		done = append(done, rel)
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
			for _, entry := range m.originals {
				entry.Unknown = false
			}
		}
		m.indexGeneration++
	}
	m.indexMu.Unlock()
	stats := m.Stats()
	m.logger.Info("cache discovery progress", slog.Bool("complete", ready),
		slog.Int("originals", stats.Originals), slog.Int("variants", stats.Variants),
		slog.Int("retry_directories", len(state.directories)), slog.Int("retry_references", len(state.references)),
		slog.Int("backing_off", len(state.abandoned)),
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
		// Asked per reference rather than once per pass: discovery over a large
		// cache runs long enough for the volume to disappear inside it, and each
		// nested mount under base_dir can be missing on its own.
		if !m.originalsAvailableFor(cacheRel) {
			return errOriginalsUnavailable
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
