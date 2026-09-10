//go:build integration

package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	scanBaseDirEnv      = "FARS_SCAN_BASE_DIR"
	scanOriginalsDirEnv = "FARS_SCAN_ORIGINALS_DIR"
	scanCacheDirEnv     = "FARS_SCAN_CACHE_DIR"
)

type scanFileSignature struct {
	size    int64
	modTime int64
}

type scanOriginal struct {
	signature scanFileSignature
	resizes   int
}

type scanCacheInventory struct {
	files       int
	uniquePaths int
	ignored     int
	geometries  map[string]struct{}
}

type scanOriginalInventory struct {
	files         int
	bytes         int64
	orphanResizes int
	statAttempts  int
}

func TestScanOriginalsAndResizes(t *testing.T) {
	baseDir := requireScanDir(t, scanBaseDirEnv)
	originalsDir := requireScanDir(t, scanOriginalsDirEnv)
	cacheDir := requireScanDir(t, scanCacheDirEnv)
	if !pathWithinRoot(baseDir, originalsDir) {
		t.Fatalf("originals directory %q is outside base directory %q", originalsDir, baseDir)
	}

	runtime.GC()
	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)

	totalStarted := time.Now()
	cacheStarted := time.Now()
	cachePaths, cacheInventory, err := scanCacheReferences(cacheDir)
	if err != nil {
		t.Fatalf("scan cached resizes: %v", err)
	}
	cacheElapsed := time.Since(cacheStarted)

	runtime.GC()
	var memoryAfterCache runtime.MemStats
	runtime.ReadMemStats(&memoryAfterCache)

	originalsStarted := time.Now()
	originals, originalInventory, err := resolveReferencedOriginals(baseDir, originalsDir, cachePaths)
	if err != nil {
		t.Fatalf("resolve referenced originals: %v", err)
	}
	originalsElapsed := time.Since(originalsStarted)

	runtime.GC()
	var memoryWithBothMaps runtime.MemStats
	runtime.ReadMemStats(&memoryWithBothMaps)
	runtime.KeepAlive(cachePaths)

	// A production monitor only needs the resolved originals after bootstrap.
	cachePaths = nil
	runtime.GC()
	var memorySteadyState runtime.MemStats
	runtime.ReadMemStats(&memorySteadyState)

	recheckStarted := time.Now()
	changed, missing, err := recheckOriginals(baseDir, originals)
	if err != nil {
		t.Fatalf("recheck originals: %v", err)
	}
	recheckElapsed := time.Since(recheckStarted)
	runtime.KeepAlive(originals)

	mappedResizes := cacheInventory.files - originalInventory.orphanResizes
	t.Logf("cache bootstrap: resizes=%d unique_cache_paths=%d geometries=%d ignored=%d elapsed=%s throughput=%.0f files/s",
		cacheInventory.files,
		cacheInventory.uniquePaths,
		len(cacheInventory.geometries),
		cacheInventory.ignored,
		cacheElapsed.Round(time.Millisecond),
		filesPerSecond(cacheInventory.files, cacheElapsed),
	)
	t.Logf("original resolution: originals=%d bytes=%s mapped_resizes=%d orphans=%d stat_attempts=%d elapsed=%s",
		originalInventory.files,
		formatScanBytes(originalInventory.bytes),
		mappedResizes,
		originalInventory.orphanResizes,
		originalInventory.statAttempts,
		originalsElapsed.Round(time.Millisecond),
	)
	t.Logf("periodic pass: originals=%d changed=%d missing=%d elapsed=%s throughput=%.0f originals/s",
		len(originals),
		changed,
		missing,
		recheckElapsed.Round(time.Millisecond),
		filesPerSecond(len(originals), recheckElapsed),
	)
	t.Logf("mapping: average_resizes_per_original=%.2f", averageResizes(mappedResizes, len(originals)))
	t.Logf("memory: cache_index=%s both_maps=%s steady_originals_map=%s",
		formatScanBytes(heapGrowth(memoryBefore.HeapAlloc, memoryAfterCache.HeapAlloc)),
		formatScanBytes(heapGrowth(memoryBefore.HeapAlloc, memoryWithBothMaps.HeapAlloc)),
		formatScanBytes(heapGrowth(memoryBefore.HeapAlloc, memorySteadyState.HeapAlloc)),
	)
	t.Logf("total bootstrap elapsed: %s", time.Since(totalStarted).Round(time.Millisecond))
	t.Logf("geometries: %s", strings.Join(sortedSetKeys(cacheInventory.geometries), ", "))
}

func requireScanDir(t *testing.T, envName string) string {
	t.Helper()

	path := strings.TrimSpace(os.Getenv(envName))
	if path == "" {
		t.Skipf("set %s to run filesystem inventory", envName)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", envName, err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		t.Fatalf("stat %s: %v", envName, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory: %s", envName, absPath)
	}
	return absPath
}

func scanCacheReferences(cacheDir string) (map[string]int, scanCacheInventory, error) {
	paths := make(map[string]int)
	inventory := scanCacheInventory{geometries: make(map[string]struct{})}

	err := filepath.WalkDir(cacheDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !isScanImage(path) {
			inventory.ignored++
			return nil
		}

		geometry, relative, ok := splitCachePath(cacheDir, path)
		if !ok {
			inventory.ignored++
			return nil
		}
		inventory.files++
		inventory.geometries[geometry] = struct{}{}
		paths[filepath.ToSlash(filepath.Clean(relative))]++
		return nil
	})
	inventory.uniquePaths = len(paths)
	return paths, inventory, err
}

func resolveReferencedOriginals(baseDir, originalsDir string, cachePaths map[string]int) (map[string]scanOriginal, scanOriginalInventory, error) {
	originals := make(map[string]scanOriginal)
	inventory := scanOriginalInventory{}

	for cacheRelative, resizeCount := range cachePaths {
		originalRelative, signature, attempts, ok, err := resolveOriginal(baseDir, originalsDir, cacheRelative)
		inventory.statAttempts += attempts
		if err != nil {
			return nil, inventory, err
		}
		if !ok {
			inventory.orphanResizes += resizeCount
			continue
		}

		original, exists := originals[originalRelative]
		if !exists {
			original.signature = signature
			inventory.files++
			inventory.bytes += signature.size
		}
		original.resizes += resizeCount
		originals[originalRelative] = original
	}
	return originals, inventory, nil
}

func resolveOriginal(baseDir, originalsDir, cacheRelative string) (string, scanFileSignature, int, bool, error) {
	candidates := []string{cacheRelative}
	withoutOutputExtension := strings.TrimSuffix(cacheRelative, filepath.Ext(cacheRelative))
	if withoutOutputExtension != cacheRelative && isScanImage(withoutOutputExtension) {
		candidates = append(candidates, withoutOutputExtension)
	}

	for index, candidate := range candidates {
		candidate = filepath.ToSlash(filepath.Clean(candidate))
		originalPath := filepath.Join(baseDir, filepath.FromSlash(candidate))
		if !pathWithinRoot(originalsDir, originalPath) {
			continue
		}

		info, err := os.Stat(originalPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", scanFileSignature{}, index + 1, false, fmt.Errorf("stat original %q: %w", originalPath, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		return candidate, scanFileSignature{size: info.Size(), modTime: info.ModTime().UnixNano()}, index + 1, true, nil
	}
	return "", scanFileSignature{}, len(candidates), false, nil
}

func recheckOriginals(baseDir string, originals map[string]scanOriginal) (changed int, missing int, err error) {
	for relative, original := range originals {
		info, statErr := os.Stat(filepath.Join(baseDir, filepath.FromSlash(relative)))
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				missing++
				continue
			}
			return changed, missing, statErr
		}
		if info.Size() != original.signature.size || info.ModTime().UnixNano() != original.signature.modTime {
			changed++
		}
	}
	return changed, missing, nil
}

func pathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func isScanImage(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".avif":
		return true
	default:
		return false
	}
}

func heapGrowth(before, after uint64) int64 {
	if after <= before {
		return 0
	}
	return int64(after - before)
}

func filesPerSecond(files int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(files) / elapsed.Seconds()
}

func averageResizes(resizes, originals int) float64 {
	if originals == 0 {
		return 0
	}
	return float64(resizes) / float64(originals)
}

func sortedSetKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func formatScanBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	divisor, exponent := int64(unit), 0
	for value := bytes / unit; value >= unit; value /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(divisor), "KMGTPE"[exponent])
}
