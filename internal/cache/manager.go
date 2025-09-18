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

	"log/slog"

	"fars/internal/config"
)

// Manager handles cache lookups and maintenance.
type Manager struct {
	cfg    *config.Config
	logger *slog.Logger
}

// NewManager creates a cache manager bound to configuration.
func NewManager(cfg *config.Config, logger *slog.Logger) *Manager {
	return &Manager{cfg: cfg, logger: logger.With("component", "cache")}
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
		return false
	}
	ttl := m.cfg.Cache.TTL.Duration
	if !originalInfo.ModTime().IsZero() && originalInfo.ModTime().After(info.ModTime()) {
		return false
	}
	if ttl > 0 && time.Since(info.ModTime()) > ttl {
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
	m.logger.Info(
		"cache cleanup finished",
		slog.Int("files_removed", stats.files),
		slog.String("bytes_removed", formatBytes(stats.bytes)),
		slog.Int64("raw_bytes_removed", stats.bytes),
	)
	return nil
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
	files int
	bytes int64
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
