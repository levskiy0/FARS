package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"log/slog"

	"fars/internal/config"
	"fars/internal/locker"
	"fars/pkg/human"
)

const originalManifestName = ".fars-originals-v1.json"

// tempFileSuffix marks in-progress publications. Such a file is never a cache
// entry, and cleanup removes the ones a crashed write left behind.
const tempFileSuffix = ".tmp"

// staleTempFileAge keeps cleanup away from publications that are still running.
const staleTempFileAge = time.Hour

// evictionBucket is the resolution of the age histogram used to pick an eviction
// cutoff without holding one record per cached file in memory.
const evictionBucket = time.Hour

// rootProbeTTL bounds how stale the cached base_dir presence check may be.
const rootProbeTTL = time.Second

// probeRetention drops cached answers about directories nothing asks about any
// more, so the probe cache cannot grow with the paths of removed mounts.
const probeRetention = time.Minute

// The originals probe descends until it finds one regular file. The budgets
// bound its cost on a network volume: originals in this deployment live a few
// levels down (img/p/1/1/11.jpg), so a populated volume answers within a handful
// of directory reads and an unpopulated one gives up quickly.
const (
	originalsProbeMaxDepth = 8
	originalsProbeMaxDirs  = 64
	originalsProbeBatch    = 128
)

// cacheHitTouchInterval rate-limits the modification-time refresh that turns
// eviction into least-recently-used ordering. See noteCacheHit.
const cacheHitTouchInterval = time.Hour

var errEmptyCacheFile = errors.New("cached file is empty")

// Manager handles cache lookups, writes, and background maintenance.
type Manager struct {
	cfg    *config.Config
	logger *slog.Logger
	locks  *locker.KeyedLocker

	background       sync.WaitGroup
	manifestMu       sync.Mutex
	indexReady       bool
	manifestLoaded   bool
	previousManifest originalManifest
	manifestWake     chan struct{}
	checkWake        chan struct{}
	pendingOriginals map[string]struct{}
	pendingOrphans   map[string]struct{}
	bootstrapMu      sync.Mutex
	bootstrapState   *bootstrapState
	sourceStat       func(string) (os.FileInfo, error)

	maxCacheSize atomic.Int64

	// Cached answers to "does this directory actually hold originals", so the
	// monitor can ask per key without probing the volume per key. See
	// probeDirectoryCached.
	probeMu sync.Mutex
	probes  map[string]directoryProbe

	indexMu             sync.RWMutex
	originals           map[string]*trackedOriginal
	bootstrapRetired    map[string]*trackedOriginal // Empty entries retained until discovery completes; guarded by indexMu.
	variantToOriginal   map[string]string
	indexGeneration     uint64
	persistedGeneration uint64
}

type trackedOriginal struct {
	Signature fileSignature
	Variants  map[string]struct{}
	// Doomed holds registered variants known to predate Signature. Invalidation
	// deletes exactly this set, so a variant published afterwards survives even
	// while older ones are still queued for removal.
	Doomed map[string]struct{}
	// Unknown marks history kept while discovery is incomplete: variants that
	// exist on disk but are not indexed yet must be treated as stale.
	Unknown bool
}

// pending reports whether anything about this original is still unresolved.
func (t *trackedOriginal) pending() bool {
	return len(t.Doomed) > 0 || t.Unknown
}

func (t *trackedOriginal) isDoomed(cachePath string) bool {
	_, ok := t.Doomed[cachePath]
	return ok
}

type fileSignature struct {
	Size        int64  `json:"size"`
	ModTimeNano int64  `json:"mtime_ns"`
	ChangeNano  int64  `json:"ctime_ns,omitempty"`
	Inode       uint64 `json:"inode,omitempty"`
}

type originalManifest struct {
	Version   int                      `json:"version"`
	Originals map[string]fileSignature `json:"originals"`
	Pending   map[string]bool          `json:"pending,omitempty"`
}

// IndexStats describes the in-memory source-to-resize index.
type IndexStats struct {
	Originals int
	Variants  int
}

// NewManager creates a cache manager bound to configuration.
func NewManager(cfg *config.Config, logger *slog.Logger) *Manager {
	m := &Manager{
		cfg:               cfg,
		logger:            logger.With("component", "cache"),
		locks:             locker.New(),
		originals:         make(map[string]*trackedOriginal),
		variantToOriginal: make(map[string]string),
		manifestWake:      make(chan struct{}, 1),
		checkWake:         make(chan struct{}, 1),
		pendingOriginals:  make(map[string]struct{}),
		pendingOrphans:    make(map[string]struct{}),
		sourceStat:        os.Stat,
	}
	if cfg != nil {
		m.SetMaxCacheSize(cfg.Cache.MaxSize.Bytes)
	}
	return m
}

// SetMaxCacheSize caps the total size of the cache directory. Zero disables
// size-based eviction, which is the behaviour of every deployment that does not
// configure a limit.
func (m *Manager) SetMaxCacheSize(limit int64) {
	if limit < 0 {
		limit = 0
	}
	m.maxCacheSize.Store(limit)
}

// LockOriginal serializes generation and invalidation for one source image.
func (m *Manager) LockOriginal(relative string) func() {
	return m.locks.Lock("original:" + filepath.ToSlash(filepath.Clean(relative)))
}

// LockCache serializes writers and removers for one cached resize.
func (m *Manager) LockCache(path string) func() {
	return m.locks.Lock("cache:" + filepath.Clean(path))
}

// EnsureParent ensures the cache directory for the target file exists.
func (m *Manager) EnsureParent(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// IsFresh determines whether cached file is still valid.
func (m *Manager) IsFresh(cachePath string, originalInfo os.FileInfo) bool {
	info, err := os.Stat(cachePath)
	// A zero-length file is never a valid image; an interrupted publication can
	// leave one behind and it must not be served as an immutable resize.
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	if originalInfo != nil {
		clean := filepath.Clean(cachePath)
		m.indexMu.RLock()
		entry := m.originals[m.variantToOriginal[clean]]
		stale := entry != nil && (entry.isDoomed(clean) || entry.Signature != signatureFromInfo(originalInfo))
		m.indexMu.RUnlock()
		if stale {
			return false
		}
	}
	ttl := m.cfg.Cache.TTL.Duration
	if originalInfo != nil && !originalInfo.ModTime().IsZero() && originalInfo.ModTime().After(info.ModTime()) {
		return false
	}
	if ttl > 0 && time.Since(info.ModTime()) > ttl {
		return false
	}
	m.noteCacheHit(cachePath, info)
	return true
}

// noteCacheHit refreshes a cached file's modification time so that eviction
// orders entries by last use instead of by creation. A cache file is never
// rewritten after publication, so its mtime is its creation time: with a size
// cap configured, eviction would otherwise discard the catalogue images every
// page loads, keep one-off crawler geometries, and re-render the catalogue on
// the next request.
//
// The refresh is deliberately narrow. It runs only when a size cap is
// configured, so deployments without one keep strict "expire N after
// publication" TTL semantics; with a cap, TTL expires entries that have not
// been served for the TTL instead, which is what an image cache wants. The
// mtime is its own rate limit: one Chtimes per cacheHitTouchInterval per file,
// so a hot path costs no extra syscall. Only a hit that was judged fresh
// touches the file, so a variant older than its original is never given a newer
// mtime than the source it was made from.
func (m *Manager) noteCacheHit(path string, info os.FileInfo) {
	if m.maxCacheSize.Load() <= 0 {
		return
	}
	now := time.Now()
	if now.Sub(info.ModTime()) < cacheHitTouchInterval {
		return
	}
	if err := os.Chtimes(path, now, now); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Warn("refresh cache entry age", slog.String("path", path), slog.Any("error", err))
	}
}

// Write stores bytes atomically and registers the resize in the source index.
// Caller holds LockOriginal followed by LockCache through generation and publication.
func (m *Manager) Write(cachePath, originalRel string, originalInfo os.FileInfo, payload []byte) error {
	if originalInfo == nil {
		return errors.New("source metadata is required")
	}
	if len(payload) == 0 {
		return errors.New("refusing to cache an empty payload")
	}
	current, err := os.Stat(filepath.Join(m.cfg.Storage.BaseDir, filepath.FromSlash(originalRel)))
	if err != nil {
		return fmt.Errorf("verify source before cache write: %w", err)
	}
	if !current.Mode().IsRegular() || signatureFromInfo(current) != signatureFromInfo(originalInfo) {
		return errors.New("source changed during resize; result was not cached")
	}
	if err := m.EnsureParent(cachePath); err != nil {
		return fmt.Errorf("ensure cache dir: %w", err)
	}
	if err := writeFileAtomic(cachePath, payload); err != nil {
		return err
	}
	m.publishVariant(originalRel, originalInfo, cachePath)
	return nil
}

// writeFileAtomic publishes payload through a uniquely named temporary file, so
// that another process sharing the cache volume cannot write into the same one,
// and flushes it before the rename, so a crash cannot publish truncated bytes.
func writeFileAtomic(path string, payload []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*"+tempFileSuffix)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := file.Name()
	closed, published := false, false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !published {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := file.Chmod(0o644); err != nil {
		return fmt.Errorf("set temp file mode: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	closed = true
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	published = true
	return nil
}

// ServeFileStats obtains file info for a cached entry.
func (m *Manager) ServeFileStats(cachePath string) (os.FileInfo, *os.File, error) {
	file, err := os.Open(cachePath)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	// Reject the truncated result of an interrupted publication instead of
	// serving zero bytes with an immutable cache header.
	if !info.Mode().IsRegular() || info.Size() == 0 {
		file.Close()
		return nil, nil, fmt.Errorf("%s: %w", cachePath, errEmptyCacheFile)
	}
	return info, file, nil
}

// StartBackground launches cleanup and original-change detection jobs.
func (m *Manager) StartBackground(ctx context.Context) {
	m.StartCleanup(ctx)
	m.StartOriginalMonitor(ctx)
}

// WaitBackground waits until all cache jobs have observed cancellation.
func (m *Manager) WaitBackground(ctx context.Context) error {
	done := make(chan struct{})
	// The helper returns as soon as the last worker does; a worker stuck in a
	// syscall on a hung volume must not cost us the inventory learned so far.
	go func() {
		m.background.Wait()
		close(done)
	}()
	var waitErr error
	select {
	case <-done:
	case <-ctx.Done():
		waitErr = ctx.Err()
	}
	return errors.Join(waitErr, m.flushManifest())
}

// StartCleanup launches periodic cleanup until the context is cancelled.
func (m *Manager) StartCleanup(ctx context.Context) {
	interval := m.cfg.Cache.CleanupInterval.Duration
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	m.background.Add(1)
	go func() {
		defer m.background.Done()
		defer ticker.Stop()
		if err := m.cleanupOnce(ctx); err != nil {
			m.logger.Error("cache cleanup failed", slog.Any("error", err))
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.cleanupOnce(ctx); err != nil {
					m.logger.Error("cache cleanup failed", slog.Any("error", err))
				}
			}
		}
	}()
}

// InvalidateOriginal removes every known resize for a relative original path.
func (m *Manager) InvalidateOriginal(ctx context.Context, originalRel string) (int, error) {
	geometries, err := m.cacheGeometries()
	if err != nil {
		return 0, err
	}
	return m.invalidateOriginal(ctx, originalRel, geometries)
}

// InvalidateOriginals invalidates a batch of originals, listing the cache
// geometries once for the whole batch instead of once per path. Counts are
// returned in input order and processing stops at the first failure, so the
// caller can tell which paths are already gone.
func (m *Manager) InvalidateOriginals(ctx context.Context, originalRels []string) ([]int, error) {
	geometries, err := m.cacheGeometries()
	if err != nil {
		return nil, err
	}
	removed := make([]int, 0, len(originalRels))
	for _, rel := range originalRels {
		count, err := m.invalidateOriginal(ctx, rel, geometries)
		if err != nil {
			return removed, err
		}
		removed = append(removed, count)
	}
	return removed, nil
}

// Only manual invalidation may discover variants not yet in the index; it passes
// the cache geometries to derive candidate paths, background invalidation nil.
func (m *Manager) invalidateOriginal(ctx context.Context, originalRel string, geometries []string) (int, error) {
	discover := geometries != nil
	clean, err := cleanRelativePath(originalRel)
	if err != nil {
		return 0, err
	}
	// Preserve an accepted manual invalidation even if its lock wait is cancelled.
	if discover {
		m.indexMu.Lock()
		if entry := m.originals[clean]; entry != nil {
			m.markPendingLocked(clean, entry)
		}
		m.indexMu.Unlock()
	}
	releaseOriginal, err := m.locks.LockContext(ctx, "original:"+clean)
	if err != nil {
		return 0, err
	}
	defer releaseOriginal()

	m.indexMu.Lock()
	entry := m.originals[clean]
	if !discover && (entry == nil || len(entry.Doomed) == 0) {
		m.indexMu.Unlock()
		return 0, nil
	}
	var variants []string
	if entry != nil {
		if discover {
			m.markPendingLocked(clean, entry)
		}
		// Only variants known to predate the current source are removed; one
		// published in the meantime is the new baseline and stays.
		variants = make([]string, 0, len(entry.Doomed))
		for path := range entry.Doomed {
			variants = append(variants, path)
		}
	}
	m.indexMu.Unlock()

	if discover {
		found, err := m.candidateVariants(geometries, clean)
		if err != nil {
			return 0, err
		}
		seen := make(map[string]bool, len(variants))
		for _, path := range variants {
			seen[path] = true
		}
		for _, path := range found {
			if !seen[path] {
				variants = append(variants, path)
				seen[path] = true
			}
		}
	}

	sort.Strings(variants)
	removed := 0
	var errs []error
	for _, path := range variants {
		if ctx.Err() != nil {
			return removed, ctx.Err()
		}
		wasRemoved, removeErr := m.removeCacheFileContext(ctx, path, nil)
		if wasRemoved {
			removed++
		}
		if removeErr != nil {
			errs = append(errs, fmt.Errorf("remove %q: %w", path, removeErr))
		}
	}
	return removed, errors.Join(errs...)
}

// cacheGeometries lists the top-level directories of the cache root. Every cache
// path is <cache_dir>/<geometry>/<original path>, so this one readdir is enough
// to enumerate the candidates for any original without walking the whole tree.
func (m *Manager) cacheGeometries() ([]string, error) {
	entries, err := os.ReadDir(m.cfg.Storage.CacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, err
	}
	geometries := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			geometries = append(geometries, entry.Name())
		}
	}
	return geometries, nil
}

// candidateVariants derives the cache paths an original can own: the geometry
// copies of the path itself plus the fallback forms carrying an extra output
// extension, and returns those that exist.
func (m *Manager) candidateVariants(geometries []string, originalRel string) ([]string, error) {
	rel := filepath.FromSlash(originalRel)
	// Only a source that is itself a cacheable image gets double-extension
	// variants, which is the ownership rule outputBase encodes.
	if !isAllowedCacheExt(originalRel) {
		return nil, nil
	}
	suffixes := make([]string, 0, len(allowedCacheExtensionList)+1)
	suffixes = append(suffixes, "")
	for _, ext := range allowedCacheExtensionList {
		// An exact double-extension source is independent of its fallback source.
		_, err := m.sourceStat(filepath.Join(m.cfg.Storage.BaseDir, rel+ext))
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		suffixes = append(suffixes, ext)
	}
	var variants []string
	for _, geometry := range geometries {
		for _, suffix := range suffixes {
			path := filepath.Join(m.cfg.Storage.CacheDir, geometry, rel+suffix)
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			variants = append(variants, path)
		}
	}
	return variants, nil
}

func outputBase(cacheRel string) string {
	trimmed := strings.TrimSuffix(cacheRel, filepath.Ext(cacheRel))
	if isAllowedCacheExt(trimmed) {
		return trimmed
	}
	return ""
}

// Stats returns a consistent snapshot of the source index size.
func (m *Manager) Stats() IndexStats {
	m.indexMu.RLock()
	defer m.indexMu.RUnlock()
	return IndexStats{Originals: len(m.originals), Variants: len(m.variantToOriginal)}
}

// trackVariant registers a variant found on disk. Its provenance is unknown, so
// the stored baseline is retained and any pending state still applies to it.
func (m *Manager) trackVariant(originalRel string, info os.FileInfo, cachePath string) {
	m.registerVariant(originalRel, info, cachePath, false)
}

// publishVariant registers a variant that was just produced from info. Those
// bytes are the new baseline, which is how an entry stops being stale without
// having to be emptied by deletion first.
func (m *Manager) publishVariant(originalRel string, info os.FileInfo, cachePath string) {
	m.registerVariant(originalRel, info, cachePath, true)
}

func (m *Manager) registerVariant(originalRel string, info os.FileInfo, cachePath string, published bool) {
	clean := filepath.ToSlash(filepath.Clean(originalRel))
	cachePath = filepath.Clean(cachePath)
	signature := signatureFromInfo(info)
	m.indexMu.Lock()
	defer m.indexMu.Unlock()
	// Exact-source creation can change ownership of a former fallback variant.
	if previous, ok := m.variantToOriginal[cachePath]; ok && previous != clean {
		if old := m.originals[previous]; old != nil {
			m.releaseVariantLocked(previous, old, cachePath)
		}
		delete(m.variantToOriginal, cachePath)
		m.indexGeneration++
	}
	entry := m.originals[clean]
	if entry == nil {
		entry = m.bootstrapRetired[clean]
		if entry == nil {
			entry = &trackedOriginal{Signature: signature, Variants: make(map[string]struct{})}
		} else {
			delete(m.bootstrapRetired, clean)
		}
		m.originals[clean] = entry
		m.indexGeneration++
	}
	if _, exists := entry.Variants[cachePath]; !exists {
		entry.Variants[cachePath] = struct{}{}
		m.variantToOriginal[cachePath] = clean
		m.indexGeneration++
	}
	if !published {
		if entry.pending() || entry.Signature != signature {
			m.markPendingLocked(clean, entry)
		}
		return
	}
	if entry.Signature != signature {
		// Everything registered before this publication came from the previous
		// source; from here on the freshly written bytes are the baseline.
		m.markPendingLocked(clean, entry)
		entry.Signature = signature
		m.indexGeneration++
	}
	m.clearDoomedLocked(clean, entry, cachePath)
}

func (m *Manager) unregisterVariant(cachePath string) {
	cachePath = filepath.Clean(cachePath)
	m.indexMu.Lock()
	defer m.indexMu.Unlock()
	rel, ok := m.variantToOriginal[cachePath]
	if !ok {
		return
	}
	delete(m.variantToOriginal, cachePath)
	if entry := m.originals[rel]; entry != nil {
		m.releaseVariantLocked(rel, entry, cachePath)
	}
	m.indexGeneration++
}

// releaseVariantLocked drops one variant from an entry. Caller holds indexMu.
func (m *Manager) releaseVariantLocked(rel string, entry *trackedOriginal, cachePath string) {
	delete(entry.Variants, cachePath)
	m.clearDoomedLocked(rel, entry, cachePath)
	if len(entry.Variants) == 0 {
		m.retireOriginalLocked(rel, entry)
	}
}

// clearDoomedLocked forgets the scheduled deletion of one variant and leaves the
// invalidation queue once nothing is left to delete. Caller holds indexMu.
func (m *Manager) clearDoomedLocked(rel string, entry *trackedOriginal, cachePath string) {
	if _, ok := entry.Doomed[cachePath]; ok {
		delete(entry.Doomed, cachePath)
		m.indexGeneration++
	}
	if len(entry.Doomed) == 0 {
		delete(m.pendingOriginals, rel)
	}
}

// retireOriginalLocked keeps discovery history out of the active checking/deletion
// queues. Otherwise removing the last known variant would lose the baseline for
// another geometry/format not yet discovered. Caller holds indexMu.
func (m *Manager) retireOriginalLocked(rel string, entry *trackedOriginal) {
	if !m.indexReady && m.cfg.Cache.CheckOriginalsInterval.Duration > 0 {
		if m.bootstrapRetired == nil {
			m.bootstrapRetired = make(map[string]*trackedOriginal)
		}
		m.bootstrapRetired[rel] = entry
	}
	delete(m.originals, rel)
	delete(m.pendingOriginals, rel)
}

func signatureFromInfo(info os.FileInfo) fileSignature {
	changeNano, inode := platformFileIdentity(info)
	return fileSignature{
		Size:        info.Size(),
		ModTimeNano: info.ModTime().UnixNano(),
		ChangeNano:  changeNano,
		Inode:       inode,
	}
}

func (m *Manager) manifestPath() string {
	return filepath.Join(m.cfg.Storage.CacheDir, originalManifestName)
}

func (m *Manager) loadManifest() (originalManifest, error) {
	payload, err := os.ReadFile(m.manifestPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return originalManifest{Originals: make(map[string]fileSignature)}, nil
		}
		return originalManifest{}, err
	}
	var manifest originalManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return originalManifest{}, err
	}
	if manifest.Version != 1 || manifest.Originals == nil {
		return originalManifest{}, fmt.Errorf("unsupported original manifest version %d", manifest.Version)
	}
	return manifest, nil
}

func (m *Manager) flushManifest() error {
	m.manifestMu.Lock()
	defer m.manifestMu.Unlock()
	m.indexMu.RLock()
	generation := m.indexGeneration
	if !m.manifestLoaded || generation == m.persistedGeneration {
		m.indexMu.RUnlock()
		return nil
	}
	originals := make(map[string]fileSignature, len(m.originals))
	pending := make(map[string]bool)
	// Retain unresolved baselines while discovery is incomplete.
	if !m.indexReady {
		for rel, signature := range m.previousManifest.Originals {
			originals[rel] = signature
		}
		for rel, value := range m.previousManifest.Pending {
			pending[rel] = value
		}
	}
	// Retired entries still protect late discoveries, including after a restart
	// during partial bootstrap. They disappear from snapshots once discovery ends.
	for _, entries := range []map[string]*trackedOriginal{m.bootstrapRetired, m.originals} {
		for rel, entry := range entries {
			if old, ok := originals[rel]; !ok {
				originals[rel] = entry.Signature
			} else if old != entry.Signature {
				pending[rel] = true
			}
			if entry.pending() {
				pending[rel] = true
			}
		}
	}
	m.indexMu.RUnlock()

	payload, err := json.Marshal(originalManifest{Version: 1, Originals: originals, Pending: pending})
	if err != nil {
		return fmt.Errorf("marshal original manifest: %w", err)
	}
	if err := m.EnsureParent(m.manifestPath()); err != nil {
		return err
	}
	tmp := m.manifestPath() + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return fmt.Errorf("write original manifest: %w", err)
	}
	if err := os.Rename(tmp, m.manifestPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace original manifest: %w", err)
	}
	m.indexMu.Lock()
	if m.persistedGeneration < generation {
		m.persistedGeneration = generation
	}
	m.indexMu.Unlock()
	return nil
}

func (m *Manager) cleanupOnce(ctx context.Context) error {
	root := m.cfg.Storage.CacheDir
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	ttl := m.cfg.Cache.TTL.Duration
	limit := m.maxCacheSize.Load()
	m.forgetProbes()
	// An unmounted or empty originals volume makes every cached file look like
	// an orphan. Skip orphan deletion rather than wipe the cache while the bind
	// mount is missing; TTL eviction is unaffected. This is only the opening
	// answer: a pass over a large cache runs for minutes, so the question is
	// asked again, per entry, before every orphan deletion below.
	orphansAllowed := m.originalsRootPopulatedCached()
	if !orphansAllowed {
		m.logger.Warn("originals directory missing or empty; skipping orphan cleanup",
			slog.String("base_dir", m.cfg.Storage.BaseDir))
	}
	m.logger.Info("cache cleanup started", slog.String("root", root))
	stats := cleanupStats{}
	usage := sizeUsage{}
	staleTempBefore := time.Now().Add(-staleTempFileAge)
	dirs := make([]string, 0, 16)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isAllowedCacheExt(path) {
			m.removeStaleTemp(path, d, staleTempBefore)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if ttl > 0 && time.Since(info.ModTime()) > ttl {
			if _, err := m.removeCacheFileIfUnchanged(ctx, path, info, &stats); err != nil {
				m.logger.Warn("remove stale cache", slog.String("path", path), slog.Any("error", err))
			}
			return nil
		}
		_, rel, ok := splitCachePath(m.cfg.Storage.CacheDir, path)
		if !ok {
			return nil
		}
		_, origInfo, err := m.resolveOriginalInfo(rel)
		if err != nil {
			// Re-ask before the deletion itself, not once for the whole pass:
			// the volume can be pulled out halfway through one. The answer is
			// cached for rootProbeTTL, so this costs a probe per second rather
			// than a probe per file.
			if errors.Is(err, os.ErrNotExist) && m.originalsAvailableFor(rel) {
				if _, remErr := m.removeCacheFileIfUnchanged(ctx, path, info, &stats); remErr != nil {
					m.logger.Warn("remove orphan cache", slog.String("path", path), slog.Any("error", remErr))
				}
				return nil
			}
			usage.add(info)
			return nil
		}
		if origInfo.ModTime().After(info.ModTime()) {
			if _, err := m.removeCacheFileIfUnchanged(ctx, path, info, &stats); err != nil {
				m.logger.Warn("remove outdated cache", slog.String("path", path), slog.Any("error", err))
			}
			return nil
		}
		usage.add(info)
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if err := m.evictBySize(ctx, limit, usage, &stats); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		if dir == root {
			continue
		}
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			m.logger.Warn("remove cache dir", slog.String("path", dir), slog.Any("error", err))
		}
	}
	m.logger.Info("cache cleanup finished",
		slog.Int("files_removed", stats.files),
		slog.String("bytes_removed", human.FormatBytes(stats.bytes)),
		slog.Int64("raw_bytes_removed", stats.bytes))
	return nil
}

// originalsRootPopulated reports whether the originals volume actually holds
// image data. Asking only whether base_dir has a dirent answers nothing here:
// the data arrives through bind mounts nested inside it (base_dir/img,
// base_dir/modules, base_dir/themes) and Docker creates those directories
// whether or not anything is mounted into them, so the root looks populated
// while the volume is empty. The probe therefore looks for a regular file,
// descending a bounded number of directories and stopping at the first hit.
func (m *Manager) originalsRootPopulated() bool {
	return m.directoryHoldsFiles(m.cfg.Storage.BaseDir)
}

// directoryHoldsFiles reports whether dir contains a regular file within the
// probe budget. Anything inconclusive - a permission error, an unreadable
// directory, or the budget running out - counts as "no": the answer only ever
// gates deletion, so an uncertain probe must never authorise one.
func (m *Manager) directoryHoldsFiles(dir string) bool {
	if dir == "" {
		return false
	}
	probe := originalsProbe{dirs: originalsProbeMaxDirs}
	found, _ := probe.scan(dir, 0)
	return found
}

// originalsProbe carries the remaining directory budget of one probe.
type originalsProbe struct {
	dirs int
}

// scan reports whether dir holds a regular file, and whether the search was cut
// short by an error or by the budget. found is authoritative; inconclusive only
// qualifies a negative answer.
func (p *originalsProbe) scan(dir string, depth int) (found bool, inconclusive bool) {
	if p.dirs <= 0 {
		return false, true
	}
	p.dirs--
	handle, err := os.Open(dir)
	if err != nil {
		return false, true
	}
	defer handle.Close()
	var subdirs []string
	for {
		entries, err := handle.ReadDir(originalsProbeBatch)
		for _, entry := range entries {
			switch {
			case entry.Type().IsRegular():
				return true, false
			case entry.Type()&os.ModeSymlink != 0:
				// resolveOriginalInfo follows symlinks, so a link to an image
				// is an original like any other.
				if target, statErr := os.Stat(filepath.Join(dir, entry.Name())); statErr == nil && target.Mode().IsRegular() {
					return true, false
				}
			case entry.IsDir() && depth < originalsProbeMaxDepth && len(subdirs) < originalsProbeMaxDirs:
				subdirs = append(subdirs, filepath.Join(dir, entry.Name()))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			inconclusive = true
			break
		}
	}
	for _, sub := range subdirs {
		subFound, subInconclusive := p.scan(sub, depth+1)
		if subFound {
			return true, false
		}
		inconclusive = inconclusive || subInconclusive
	}
	return false, inconclusive
}

// directoryProbe is one remembered probe answer.
type directoryProbe struct {
	at time.Time
	ok bool
}

// probeDirectoryCached answers directoryHoldsFiles at most once per
// rootProbeTTL per directory. The monitor asks per key on every pass, and
// probing the volume per key is the kind of cost that made the previous
// full-tree walks a problem.
func (m *Manager) probeDirectoryCached(dir string) bool {
	now := time.Now()
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	if cached, seen := m.probes[dir]; seen && now.Sub(cached.at) < rootProbeTTL {
		return cached.ok
	}
	ok := m.directoryHoldsFiles(dir)
	if previous, seen := m.probes[dir]; !seen || previous.ok != ok {
		if !ok {
			m.logger.Warn("originals directory holds no readable image; invalidation and orphan removal are paused",
				"dir", dir, "base_dir", m.cfg.Storage.BaseDir)
		} else if seen {
			m.logger.Info("originals directory holds images again", "dir", dir)
		}
	}
	if m.probes == nil {
		m.probes = make(map[string]directoryProbe)
	}
	for key, entry := range m.probes {
		if now.Sub(entry.at) > probeRetention {
			delete(m.probes, key)
		}
	}
	m.probes[dir] = directoryProbe{at: now, ok: ok}
	return ok
}

func (m *Manager) originalsRootPopulatedCached() bool {
	return m.probeDirectoryCached(m.cfg.Storage.BaseDir)
}

// forgetProbes drops the remembered answers, so a pass that runs for minutes
// opens with a fresh look at the volume instead of one left over from the
// previous pass. Within a pass the cache still bounds the cost.
func (m *Manager) forgetProbes() {
	m.probeMu.Lock()
	clear(m.probes)
	m.probeMu.Unlock()
}

// originalsAvailableFor reports whether the part of the volume that one
// relative original belongs to is there. The root and the branch are asked
// separately because base_dir/img, base_dir/modules and base_dir/themes are
// separate bind mounts: the root can hold files while the one mount this entry
// depends on is missing.
func (m *Manager) originalsAvailableFor(rel string) bool {
	if !m.originalsRootPopulatedCached() {
		return false
	}
	branch := originalsBranch(rel)
	if branch == "" {
		return true
	}
	return m.probeDirectoryCached(filepath.Join(m.cfg.Storage.BaseDir, branch))
}

// originalsBranch returns the first path segment of a relative original, which
// is the granularity at which this deployment mounts the originals volume. It
// is empty for a file that sits directly in base_dir.
func originalsBranch(rel string) string {
	clean, err := cleanRelativePath(rel)
	if err != nil {
		return ""
	}
	first, _, ok := strings.Cut(clean, "/")
	if !ok {
		return ""
	}
	return first
}

// removeStaleTemp deletes what a crashed or failed publication left behind.
// Such files are invisible to the cache index and would otherwise accumulate
// and keep their directories from being pruned.
func (m *Manager) removeStaleTemp(path string, d fs.DirEntry, before time.Time) {
	if !strings.HasSuffix(path, tempFileSuffix) || d.Type()&os.ModeSymlink != 0 {
		return
	}
	info, err := d.Info()
	if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(before) {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Warn("remove abandoned temp file", slog.String("path", path), slog.Any("error", err))
	}
}

// sizeUsage accumulates the cache footprint as an age histogram, so that a cap
// can be enforced over millions of files without keeping a record per file.
type sizeUsage struct {
	total   int64
	buckets map[int64]int64
}

func (u *sizeUsage) add(info os.FileInfo) {
	u.total += info.Size()
	if u.buckets == nil {
		u.buckets = make(map[int64]int64)
	}
	u.buckets[info.ModTime().Unix()/int64(evictionBucket/time.Second)] += info.Size()
}

// cutoff returns the modification time below which deleting every cached file
// frees at least need bytes.
func (u *sizeUsage) cutoff(need int64) time.Time {
	ages := make([]int64, 0, len(u.buckets))
	for bucket := range u.buckets {
		ages = append(ages, bucket)
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i] < ages[j] })
	freed := int64(0)
	for _, bucket := range ages {
		freed += u.buckets[bucket]
		if freed >= need {
			return time.Unix((bucket+1)*int64(evictionBucket/time.Second), 0)
		}
	}
	return time.Now()
}

// evictBySize removes the least recently used cached files until the cache fits
// the cap. Eviction is by modification time: container volumes are commonly
// mounted with relatime or noatime, so access times are not a dependable
// recency signal. noteCacheHit keeps that modification time meaning "last
// served" whenever a cap is configured, which is what makes this order LRU
// rather than "oldest published first".
func (m *Manager) evictBySize(ctx context.Context, limit int64, usage sizeUsage, stats *cleanupStats) error {
	if limit <= 0 || usage.total <= limit {
		return nil
	}
	need := usage.total - limit
	cutoff := usage.cutoff(need)
	m.logger.Warn("cache size over the configured cap; evicting oldest entries",
		slog.String("size", human.FormatBytes(usage.total)),
		slog.String("limit", human.FormatBytes(limit)),
		slog.String("to_free", human.FormatBytes(need)),
		slog.Time("older_than", cutoff))
	freed := int64(0)
	before := stats.bytes
	walkErr := filepath.WalkDir(m.cfg.Storage.CacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isAllowedCacheExt(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			return nil
		}
		if _, err := m.removeCacheFileIfUnchanged(ctx, path, info, stats); err != nil {
			m.logger.Warn("evict cache entry", slog.String("path", path), slog.Any("error", err))
			return nil
		}
		freed = stats.bytes - before
		if freed >= need {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	m.logger.Info("cache size eviction finished",
		slog.String("freed", human.FormatBytes(freed)),
		slog.String("size", human.FormatBytes(usage.total-freed)),
		slog.String("limit", human.FormatBytes(limit)))
	return nil
}

func splitCachePath(cacheRoot, candidate string) (geometry string, rel string, ok bool) {
	relPath, err := filepath.Rel(cacheRoot, candidate)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(filepath.ToSlash(relPath), "/", 2)
	if len(parts) != 2 || parts[0] == ".." || parts[0] == "." || filepath.IsAbs(relPath) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type cleanupStats struct {
	files int
	bytes int64
}

var allowedCacheExtensions = map[string]struct{}{
	".png":  {},
	".avif": {},
	".webp": {},
	".jpg":  {},
	".jpeg": {},
}

// allowedCacheExtensionList is the deterministic form of allowedCacheExtensions.
var allowedCacheExtensionList = func() []string {
	list := make([]string, 0, len(allowedCacheExtensions))
	for ext := range allowedCacheExtensions {
		list = append(list, ext)
	}
	sort.Strings(list)
	return list
}()

func isAllowedCacheExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	_, ok := allowedCacheExtensions[ext]
	return ok
}

func (m *Manager) resolveOriginalInfo(rel string) (string, os.FileInfo, error) {
	clean, err := cleanRelativePath(rel)
	if err != nil {
		return "", nil, err
	}
	info, err := m.sourceStat(filepath.Join(m.cfg.Storage.BaseDir, filepath.FromSlash(clean)))
	if err == nil {
		if !info.Mode().IsRegular() {
			return "", nil, os.ErrNotExist
		}
		return clean, info, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	trimmed := outputBase(clean)
	if trimmed == "" {
		return "", nil, err
	}
	info, fallbackErr := m.sourceStat(filepath.Join(m.cfg.Storage.BaseDir, filepath.FromSlash(trimmed)))
	if fallbackErr == nil {
		if !info.Mode().IsRegular() {
			return "", nil, os.ErrNotExist
		}
		return trimmed, info, nil
	}
	if errors.Is(fallbackErr, os.ErrNotExist) {
		return "", nil, err
	}
	return "", nil, fallbackErr
}

func cleanRelativePath(rel string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(rel, "/")))
	if filepath.IsAbs(rel) || strings.ContainsRune(rel, 0) || clean == "." || clean == "" || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("invalid original path")
	}
	return clean, nil
}

// removeCacheFileIfUnchanged discards cleanup decisions made about a previous
// version of a file. The observation is revalidated under the publication lock;
// replacements are left for a later cleanup pass to assess on their own merits.
func (m *Manager) removeCacheFileIfUnchanged(ctx context.Context, path string, observed os.FileInfo, stats *cleanupStats) (bool, error) {
	release, err := m.locks.LockContext(ctx, "cache:"+filepath.Clean(path))
	if err != nil {
		return false, err
	}
	defer release()
	current, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.unregisterVariant(path)
			return false, nil
		}
		return false, err
	}
	if !os.SameFile(observed, current) || observed.Mode() != current.Mode() || signatureFromInfo(observed) != signatureFromInfo(current) {
		return false, nil
	}
	return m.removeCacheFileLocked(path, stats)
}

func (m *Manager) removeCacheFileContext(ctx context.Context, path string, stats *cleanupStats) (bool, error) {
	release, err := m.locks.LockContext(ctx, "cache:"+filepath.Clean(path))
	if err != nil {
		return false, err
	}
	defer release()
	return m.removeCacheFileLocked(path, stats)
}

// Caller must hold LockCache(path) until removal and index updates complete.
func (m *Manager) removeCacheFileLocked(path string, stats *cleanupStats) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.unregisterVariant(path)
			return false, nil
		}
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			m.unregisterVariant(path)
			return false, nil
		}
		return false, err
	}
	m.unregisterVariant(path)
	if stats != nil {
		stats.files++
		stats.bytes += info.Size()
	}
	return true, nil
}
