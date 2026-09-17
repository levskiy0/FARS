package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateRefusesToCreateMissingBaseDir pins the guard that keeps an
// unmounted bind mount from masquerading as an empty, healthy originals
// directory: base_dir must already exist, and validation must not create it.
func TestValidateRefusesToCreateMissingBaseDir(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "not-mounted")
	cfg := validConfig(t)
	cfg.Storage.BaseDir = missing

	err := cfg.Validate()
	if err == nil {
		t.Fatal("accepted a base_dir that does not exist")
	}
	if !strings.Contains(err.Error(), "storage.base_dir") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not identify the missing base_dir", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("validation created storage.base_dir %s (stat error: %v)", missing, statErr)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("validation wrote into the base_dir parent: %v", entries)
	}
}

func TestValidateRejectsBaseDirThatIsAFile(t *testing.T) {
	cfg := validConfig(t)
	file := filepath.Join(t.TempDir(), "originals")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Storage.BaseDir = file
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected a not-a-directory error, got %v", err)
	}
}

func TestValidateCreatesMissingCacheDir(t *testing.T) {
	cfg := validConfig(t)
	cacheDir := filepath.Join(t.TempDir(), "nested", "cache")
	cfg.Storage.CacheDir = cacheDir
	if err := cfg.Validate(); err != nil {
		t.Fatalf("rejected a creatable cache_dir: %v", err)
	}
	info, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatalf("cache_dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("cache_dir is not a directory: %v", info.Mode())
	}
}

func TestValidateRejectsCacheDirThatIsAFile(t *testing.T) {
	cfg := validConfig(t)
	file := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Storage.CacheDir = file
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected a not-a-directory error, got %v", err)
	}
}

func TestValidateRejectsStorageContainment(t *testing.T) {
	root := string(filepath.Separator)
	cases := []struct {
		name  string
		build func(t *testing.T) (baseDir, cacheDir string)
		want  string
	}{
		{
			name: "cache_dir is the filesystem root",
			build: func(t *testing.T) (string, string) {
				return t.TempDir(), root
			},
			want: "filesystem root",
		},
		{
			name: "base_dir is the filesystem root",
			build: func(t *testing.T) (string, string) {
				return root, filepath.Join(t.TempDir(), "cache")
			},
			want: "filesystem root",
		},
		{
			name: "cache_dir inside base_dir",
			build: func(t *testing.T) (string, string) {
				base := t.TempDir()
				return base, filepath.Join(base, "cache")
			},
			want: "must not be inside",
		},
		{
			name: "base_dir inside cache_dir",
			build: func(t *testing.T) (string, string) {
				cache := t.TempDir()
				base := filepath.Join(cache, "originals")
				if err := os.MkdirAll(base, 0o755); err != nil {
					t.Fatal(err)
				}
				return base, cache
			},
			want: "must not be inside",
		},
		{
			name: "identical directories",
			build: func(t *testing.T) (string, string) {
				shared := t.TempDir()
				return shared, shared
			},
			want: "must not be the same directory",
		},
		{
			name: "base_dir symlink hiding containment",
			build: func(t *testing.T) (string, string) {
				real := t.TempDir()
				link := filepath.Join(t.TempDir(), "base-link")
				if err := os.Symlink(real, link); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				return link, filepath.Join(real, "cache")
			},
			want: "must not be inside",
		},
		{
			name: "cache_dir symlink hiding equality",
			build: func(t *testing.T) (string, string) {
				real := t.TempDir()
				link := filepath.Join(t.TempDir(), "cache-link")
				if err := os.Symlink(real, link); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				return real, link
			},
			want: "must not be the same directory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseDir, cacheDir := tc.build(t)
			cfg := validConfig(t)
			cfg.Storage.BaseDir = baseDir
			cfg.Storage.CacheDir = cacheDir
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("accepted base_dir %q with cache_dir %q", baseDir, cacheDir)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not explain the containment failure (want %q)", err, tc.want)
			}
			// The operator has to be able to tell which pair is at fault.
			switch tc.want {
			case "filesystem root":
				if !strings.Contains(err.Error(), root) {
					t.Fatalf("error %q does not name the offending path", err)
				}
			default:
				if !strings.Contains(err.Error(), baseDir) || !strings.Contains(err.Error(), cacheDir) {
					t.Fatalf("error %q does not name both %q and %q", err, baseDir, cacheDir)
				}
			}
			if _, statErr := os.Stat(cacheDir); tc.name == "cache_dir inside base_dir" && !os.IsNotExist(statErr) {
				t.Fatalf("rejected cache_dir was created anyway: %v", statErr)
			}
		})
	}
}

func TestValidateAcceptsSiblingStorageDirectories(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, "originals")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := validConfig(t)
	cfg.Storage.BaseDir = base
	cfg.Storage.CacheDir = filepath.Join(parent, "cache")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("rejected sibling storage directories: %v", err)
	}
}
