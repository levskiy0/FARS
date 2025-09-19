package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"container/list"
	"sync"

	"log/slog"

	"fars/internal/config"
)

// Manager handles cache lookups and maintenance.
type Manager struct {
	cfg    *config.Config
	logger *slog.Logger
	memory *memoryCache
	hot    *hotCache
}

// MemoryResult represents a payload retrieved from the in-memory hot cache.
type MemoryResult struct {
	Payload []byte
	ModTime time.Time
	Size    int64
}

// NewManager creates a cache manager bound to configuration.
func NewManager(cfg *config.Config, logger *slog.Logger) *Manager {
	m := &Manager{cfg: cfg, logger: logger.With("component", "cache")}
	if limit := cfg.Cache.MemoryCacheSize.Bytes; limit > 0 {
		chunk := cfg.Cache.MaxMemoryChunk.Bytes
		if chunk <= 0 || chunk > limit {
			chunk = limit
		}
		m.memory = newMemoryCache(limit, chunk)
	}
	if hot := cfg.Cache.StorageHotCacheSize.Bytes; hot > 0 {
		m.hot = newHotCache(hot)
	}
	return m
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
	if err != nil {
		m.evictMemory(cachePath)
		return false
	}
	ttl := m.cfg.Cache.TTL.Duration
	if originalInfo != nil && !originalInfo.ModTime().IsZero() && originalInfo.ModTime().After(info.ModTime()) {
		m.evictMemory(cachePath)
		return false
	}
	if ttl > 0 && time.Since(info.ModTime()) > ttl {
		m.evictMemory(cachePath)
		return false
	}
	return true
}

// Write stores bytes to cache respecting file permissions.
func (m *Manager) Write(cachePath string, payload []byte) error {
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
	if m.memory != nil {
		if info, err := os.Stat(cachePath); err == nil {
			m.memory.store(cachePath, payload, info.ModTime())
			m.markHot(cachePath, int64(len(payload)))
		}
	}
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

// LoadMemory returns a cached payload from memory when it is still fresh.
func (m *Manager) LoadMemory(cachePath string, originalInfo os.FileInfo) (*MemoryResult, bool) {
	if m.memory == nil {
		return nil, false
	}
	var origMod time.Time
	if originalInfo != nil {
		origMod = originalInfo.ModTime()
	}
	entry := m.memory.load(cachePath, origMod, m.cfg.Cache.TTL.Duration)
	if entry == nil {
		return nil, false
	}
	m.markHot(cachePath, entry.size)
	return &MemoryResult{
		Payload: entry.payload,
		ModTime: entry.modTime,
		Size:    entry.size,
	}, true
}

// MarkHot records that a cache path was recently accessed so cleanup can protect it.
func (m *Manager) MarkHot(cachePath string, size int64) {
	m.markHot(cachePath, size)
}

// StartCleanup launches periodic cleanup until the context is cancelled.
func (m *Manager) StartCleanup(ctx context.Context) {
	interval := m.cfg.Cache.CleanupInterval.Duration
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	go func() {
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
			return err
		}
		if ttl > 0 && time.Since(info.ModTime()) > ttl {
			if m.shouldKeepHot(path, info.Size(), &stats) {
				return nil
			}
			if err := m.removeCacheFile(path, info.Size(), &stats); err != nil {
				m.logger.Warn("remove stale cache", slog.String("path", path), slog.Any("error", err))
			}
			return nil
		}
		_, rel, ok := splitCachePath(m.cfg.Storage.CacheDir, path)
		if !ok {
			return nil
		}
		origInfo, err := m.lookupOriginalInfo(rel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if remErr := m.removeCacheFile(path, info.Size(), &stats); remErr != nil {
					m.logger.Warn("remove orphan cache", slog.String("path", path), slog.Any("error", remErr))
				}
			}
			return nil
		}
		if origInfo.ModTime().After(info.ModTime()) {
			if err := m.removeCacheFile(path, info.Size(), &stats); err != nil {
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
	if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		m.logger.Warn("remove cache root", slog.String("path", root), slog.Any("error", err))
	}
	attrs := []any{
		slog.Int("files_removed", stats.files),
		slog.String("bytes_removed", formatBytes(stats.bytes)),
		slog.Int64("raw_bytes_removed", stats.bytes),
	}
	if stats.hotPreserved > 0 {
		attrs = append(attrs,
			slog.Int("hot_retained", stats.hotPreserved),
			slog.String("hot_bytes_retained", formatBytes(stats.hotBytes)),
			slog.Int64("raw_hot_bytes_retained", stats.hotBytes),
		)
	}
	m.logger.Info("cache cleanup finished", attrs...)
	return nil
}

func (m *Manager) shouldKeepHot(path string, size int64, stats *cleanupStats) bool {
	if m.hot == nil || size <= 0 {
		return false
	}
	if !m.hot.protect(path, size) {
		return false
	}
	if stats != nil {
		stats.hotPreserved++
		stats.hotBytes += size
	}
	return true
}

func splitCachePath(cacheRoot, candidate string) (geometry string, rel string, ok bool) {
	relPath, err := filepath.Rel(cacheRoot, candidate)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(filepath.ToSlash(relPath), "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type cleanupStats struct {
	files        int
	bytes        int64
	hotPreserved int
	hotBytes     int64
}

var allowedCacheExtensions = map[string]struct{}{
	".png":  {},
	".avif": {},
	".webp": {},
	".jpg":  {},
}

func isAllowedCacheExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	_, ok := allowedCacheExtensions[ext]
	return ok
}

func (m *Manager) lookupOriginalInfo(rel string) (os.FileInfo, error) {
	originalPath := filepath.Join(m.cfg.Storage.BaseDir, rel)
	info, err := os.Stat(originalPath)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	trimmed := strings.TrimSuffix(rel, filepath.Ext(rel))
	if trimmed == rel {
		return nil, err
	}
	fallbackPath := filepath.Join(m.cfg.Storage.BaseDir, trimmed)
	info, fallbackErr := os.Stat(fallbackPath)
	if fallbackErr == nil {
		return info, nil
	}
	if errors.Is(fallbackErr, os.ErrNotExist) {
		return nil, err
	}
	return nil, fallbackErr
}

func (m *Manager) removeCacheFile(path string, size int64, stats *cleanupStats) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	stats.files++
	stats.bytes += size
	m.drop(path)
	return nil
}

func formatBytes(n int64) string {
	if n == 0 {
		return "0 B"
	}
	sizes := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	value := float64(n)
	idx := 0
	for value >= 1024 && idx < len(sizes)-1 {
		value /= 1024
		idx++
	}
	if idx == 0 {
		return fmt.Sprintf("%d %s", n, sizes[idx])
	}
	return fmt.Sprintf("%.2f %s", value, sizes[idx])
}

func (m *Manager) markHot(path string, size int64) {
	if m.hot == nil || size <= 0 {
		return
	}
	m.hot.mark(path, size)
}

func (m *Manager) evictMemory(path string) {
	if m.memory == nil {
		return
	}
	m.memory.remove(path)
}

func (m *Manager) drop(path string) {
	if m.memory != nil {
		m.memory.remove(path)
	}
	if m.hot != nil {
		m.hot.remove(path)
	}
}

type memoryCache struct {
	limit int64
	chunk int64
	used  int64
	items map[string]*list.Element
	order *list.List
	mu    sync.Mutex
}

type memoryEntry struct {
	key     string
	payload []byte
	size    int64
	modTime time.Time
}

func newMemoryCache(limit, chunk int64) *memoryCache {
	return &memoryCache{
		limit: limit,
		chunk: chunk,
		items: make(map[string]*list.Element),
		order: list.New(),
	}
}

func (c *memoryCache) store(key string, payload []byte, modTime time.Time) {
	if c == nil || c.limit <= 0 {
		return
	}
	size := int64(len(payload))
	if size == 0 {
		c.remove(key)
		return
	}
	if c.chunk > 0 && size > c.chunk {
		return
	}
	if size > c.limit {
		return
	}
	copyPayload := append([]byte(nil), payload...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*memoryEntry)
		c.used -= entry.size
		c.order.Remove(elem)
		delete(c.items, key)
	}
	entry := &memoryEntry{
		key:     key,
		payload: copyPayload,
		size:    size,
		modTime: modTime,
	}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
	c.used += size
	c.enforceLimitLocked()
}

func (c *memoryCache) load(key string, originMod time.Time, ttl time.Duration) *memoryEntry {
	if c == nil || c.limit <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return nil
	}
	entry := elem.Value.(*memoryEntry)
	if !originMod.IsZero() && originMod.After(entry.modTime) {
		c.removeElement(elem)
		return nil
	}
	if ttl > 0 && time.Since(entry.modTime) > ttl {
		c.removeElement(elem)
		return nil
	}
	c.order.MoveToFront(elem)
	return entry
}

func (c *memoryCache) remove(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return
	}
	c.removeElement(elem)
}

func (c *memoryCache) removeElement(elem *list.Element) {
	if elem == nil {
		return
	}
	entry := elem.Value.(*memoryEntry)
	delete(c.items, entry.key)
	c.order.Remove(elem)
	c.used -= entry.size
	if c.used < 0 {
		c.used = 0
	}
}

func (c *memoryCache) enforceLimitLocked() {
	for c.limit > 0 && c.used > c.limit {
		back := c.order.Back()
		if back == nil {
			return
		}
		c.removeElement(back)
	}
}

type hotCache struct {
	limit int64
	used  int64
	items map[string]*list.Element
	order *list.List
	mu    sync.Mutex
}

type hotEntry struct {
	key  string
	size int64
}

func newHotCache(limit int64) *hotCache {
	return &hotCache{
		limit: limit,
		items: make(map[string]*list.Element),
		order: list.New(),
	}
}

func (h *hotCache) mark(key string, size int64) {
	if h == nil || h.limit <= 0 || size <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.upsertLocked(key, size)
}

func (h *hotCache) protect(key string, size int64) bool {
	if h == nil || h.limit <= 0 || size <= 0 {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	elem, ok := h.items[key]
	if !ok {
		return false
	}
	entry := elem.Value.(*hotEntry)
	if size != entry.size {
		h.used += size - entry.size
		entry.size = size
	}
	h.order.MoveToFront(elem)
	h.enforceLimitLocked()
	return true
}

func (h *hotCache) upsertLocked(key string, size int64) {
	if elem, ok := h.items[key]; ok {
		entry := elem.Value.(*hotEntry)
		h.used += size - entry.size
		entry.size = size
		h.order.MoveToFront(elem)
	} else {
		elem := h.order.PushFront(&hotEntry{key: key, size: size})
		h.items[key] = elem
		h.used += size
	}
	h.enforceLimitLocked()
}

func (h *hotCache) remove(key string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	elem, ok := h.items[key]
	if !ok {
		return
	}
	entry := elem.Value.(*hotEntry)
	delete(h.items, key)
	h.order.Remove(elem)
	h.used -= entry.size
	if h.used < 0 {
		h.used = 0
	}
}

func (h *hotCache) enforceLimitLocked() {
	for h.limit > 0 && h.used > h.limit {
		back := h.order.Back()
		if back == nil {
			return
		}
		entry := back.Value.(*hotEntry)
		delete(h.items, entry.key)
		h.order.Remove(back)
		h.used -= entry.size
	}
	if h.used < 0 {
		h.used = 0
	}
}
