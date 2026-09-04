package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"log/slog"

	"fars/internal/config"
	"fars/internal/locker"
	"fars/pkg/human"
)

const originalManifestName = ".fars-originals-v1.json"

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

	indexMu             sync.RWMutex
	originals           map[string]*trackedOriginal
	bootstrapRetired    map[string]*trackedOriginal // Empty entries retained until discovery completes; guarded by indexMu.
	variantToOriginal   map[string]string
	indexGeneration     uint64
	persistedGeneration uint64
}

type trackedOriginal struct {
	Signature fileSignature
	Pending   bool
	Variants  map[string]struct{}
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
	return &Manager{
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
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if originalInfo != nil {
		m.indexMu.RLock()
		entry := m.originals[m.variantToOriginal[filepath.Clean(cachePath)]]
		stale := entry != nil && (entry.Pending || entry.Signature != signatureFromInfo(originalInfo))
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
	return true
}

// Write stores bytes atomically and registers the resize in the source index.
// Caller holds LockOriginal followed by LockCache through generation and publication.
func (m *Manager) Write(cachePath, originalRel string, originalInfo os.FileInfo, payload []byte) error {
	if originalInfo == nil {
		return errors.New("source metadata is required")
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
	tmp := cachePath + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := os.Rename(tmp, cachePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp file: %w", err)
	}
	m.trackVariant(originalRel, originalInfo, cachePath)
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
	go func() {
		m.background.Wait()
		close(done)
	}()
	select {
	case <-done:
		return m.flushManifest()
	case <-ctx.Done():
		return ctx.Err()
	}
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
	return m.invalidateOriginal(ctx, originalRel, true)
}

// Only manual invalidation may discover variants not yet in the index.
func (m *Manager) invalidateOriginal(ctx context.Context, originalRel string, discover bool) (int, error) {
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
	if !discover && (entry == nil || !entry.Pending) {
		m.indexMu.Unlock()
		return 0, nil
	}
	var variants []string
	if entry != nil {
		variants = make([]string, 0, len(entry.Variants))
		for path := range entry.Variants {
			variants = append(variants, path)
		}
		m.markPendingLocked(clean, entry)
	}
	ready := m.indexReady
	m.indexMu.Unlock()

	if discover && (!ready || len(variants) == 0) {
		var found []string
		found, err = m.findVariantsForOriginal(ctx, clean)
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
		if err != nil {
			return 0, err
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

func (m *Manager) findVariantsForOriginal(ctx context.Context, originalRel string) ([]string, error) {
	var variants []string
	err := filepath.WalkDir(m.cfg.Storage.CacheDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isAllowedCacheExt(path) {
			return nil
		}
		_, cacheRel, ok := splitCachePath(m.cfg.Storage.CacheDir, path)
		if !ok {
			return nil
		}
		cacheRel = filepath.ToSlash(filepath.Clean(cacheRel))
		if cacheRel == originalRel {
			variants = append(variants, path)
		} else if outputBase(cacheRel) == originalRel {
			// An exact double-extension source is independent of its fallback source.
			_, err := os.Stat(filepath.Join(m.cfg.Storage.BaseDir, filepath.FromSlash(cacheRel)))
			if errors.Is(err, os.ErrNotExist) {
				variants = append(variants, path)
			} else if err != nil {
				return err
			}
		}
		return nil
	})
	return variants, err
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

func (m *Manager) trackVariant(originalRel string, info os.FileInfo, cachePath string) {
	clean := filepath.ToSlash(filepath.Clean(originalRel))
	cachePath = filepath.Clean(cachePath)
	m.indexMu.Lock()
	defer m.indexMu.Unlock()
	// Exact-source creation can change ownership of a former fallback variant.
	if previous, ok := m.variantToOriginal[cachePath]; ok && previous != clean {
		if old := m.originals[previous]; old != nil {
			delete(old.Variants, cachePath)
			if len(old.Variants) == 0 {
				m.retireOriginalLocked(previous, old)
			}
		}
		delete(m.variantToOriginal, cachePath)
		m.indexGeneration++
	}
	entry := m.originals[clean]
	if entry == nil {
		entry = m.bootstrapRetired[clean]
		if entry == nil {
			entry = &trackedOriginal{Signature: signatureFromInfo(info), Variants: make(map[string]struct{})}
		} else {
			delete(m.bootstrapRetired, clean)
		}
		m.originals[clean] = entry
	}
	if entry.Pending || entry.Signature != signatureFromInfo(info) {
		m.markPendingLocked(clean, entry)
	}
	// Retain the oldest baseline until all dependent variants have been invalidated.
	if _, exists := entry.Variants[cachePath]; exists {
		return
	}
	entry.Variants[cachePath] = struct{}{}
	m.variantToOriginal[cachePath] = clean
	m.indexGeneration++
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
		delete(entry.Variants, cachePath)
		if len(entry.Variants) == 0 {
			m.retireOriginalLocked(rel, entry)
		}
	}
	m.indexGeneration++
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
			if entry.Pending {
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
	m.logger.Info("cache cleanup started", slog.String("root", root))
	stats := cleanupStats{}
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
			if errors.Is(err, os.ErrNotExist) {
				if _, remErr := m.removeCacheFileIfUnchanged(ctx, path, info, &stats); remErr != nil {
					m.logger.Warn("remove orphan cache", slog.String("path", path), slog.Any("error", remErr))
				}
			}
			return nil
		}
		if origInfo.ModTime().After(info.ModTime()) {
			if _, err := m.removeCacheFileIfUnchanged(ctx, path, info, &stats); err != nil {
				m.logger.Warn("remove outdated cache", slog.String("path", path), slog.Any("error", err))
			}
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
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

func (m *Manager) removeCacheFile(path string, stats *cleanupStats) (bool, error) {
	return m.removeCacheFileContext(context.Background(), path, stats)
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
