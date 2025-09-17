package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if ttl > 0 && time.Since(info.ModTime()) > ttl {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				m.logger.Warn("remove stale cache", slog.String("path", path), slog.Any("error", removeErr))
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, rel, ok := splitCachePath(m.cfg.Storage.CacheDir, path)
		if !ok {
			return nil
		}
		origPath := filepath.Join(m.cfg.Storage.BaseDir, rel)
		origInfo, err := os.Stat(origPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					m.logger.Warn("remove orphan cache", slog.String("path", path), slog.Any("error", removeErr))
				}
			}
			return nil
		}
		if origInfo.ModTime().After(info.ModTime()) {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				m.logger.Warn("remove outdated cache", slog.String("path", path), slog.Any("error", removeErr))
			}
		}
		return nil
	})
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
