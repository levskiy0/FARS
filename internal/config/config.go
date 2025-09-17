package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	errEmptyConfigPath      = errors.New("config path is empty")
	errInvalidGeometryLimit = errors.New("resize max dimensions must be positive")
)

// Config represents the full service configuration loaded from YAML.
type Config struct {
	Server   ServerConfig  `yaml:"server"`
	Storage  StorageConfig `yaml:"storage"`
	Resize   ResizeConfig  `yaml:"resize"`
	Cache    CacheConfig   `yaml:"cache"`
	Rewrites []RewriteRule `yaml:"rewrites"`
}

// ServerConfig describes HTTP server binding parameters.
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// Address returns the server listen address in host:port form.
func (s ServerConfig) Address() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// StorageConfig includes directories for originals and cache outputs.
type StorageConfig struct {
	BaseDir  string `yaml:"base_dir"`
	CacheDir string `yaml:"cache_dir"`
}

// ResizeConfig combines resize limits and encoding parameters.
type ResizeConfig struct {
	MaxWidth       int `yaml:"max_width"`
	MaxHeight      int `yaml:"max_height"`
	JPGQuality     int `yaml:"jpg_quality"`
	WebPQuality    int `yaml:"webp_quality"`
	AVIFQuality    int `yaml:"avif_quality"`
	PNGCompression int `yaml:"png_compression"`
}

// CacheConfig stores cache retention settings.
type CacheConfig struct {
	TTL             Duration `yaml:"ttl"`
	CleanupInterval Duration `yaml:"cleanup_interval"`
}

// Duration wraps time.Duration to support YAML strings like "30d".
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return nil
	}
	var raw string
	switch value.Kind {
	case yaml.ScalarNode:
		raw = strings.TrimSpace(value.Value)
	default:
		return fmt.Errorf("duration must be a string, got kind %d", value.Kind)
	}
	if raw == "" || strings.EqualFold(raw, "null") {
		d.Duration = 0
		return nil
	}
	dur, err := parseFlexibleDuration(raw)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

// RewriteRule mirrors nginx-style regex rewrite.
type RewriteRule struct {
	Pattern     string         `yaml:"pattern"`
	Replacement string         `yaml:"replacement"`
	re          *regexp.Regexp `yaml:"-"`
}

// Apply returns true when the rule matched and updates the target string.
func (r *RewriteRule) Apply(input string) (string, bool) {
	if r.re == nil {
		return input, false
	}
	if !r.re.MatchString(input) {
		return input, false
	}
	return r.re.ReplaceAllString(input, r.Replacement), true
}

// Load reads and validates configuration from the provided file path.
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errEmptyConfigPath
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	return LoadReader(file)
}

// LoadReader decodes configuration from an arbitrary reader.
func LoadReader(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.compile(); err != nil {
		return nil, err
	}
	return &cfg, cfg.Validate()
}

// Validate returns an error if required configuration values are missing or invalid.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.Host) == "" {
		return errors.New("server.host must be set")
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", c.Server.Port)
	}
	if strings.TrimSpace(c.Storage.BaseDir) == "" {
		return errors.New("storage.base_dir must be set")
	}
	if strings.TrimSpace(c.Storage.CacheDir) == "" {
		return errors.New("storage.cache_dir must be set")
	}
	if err := ensureDirExists(c.Storage.BaseDir); err != nil {
		return fmt.Errorf("validate storage.base_dir: %w", err)
	}
	if err := ensureDirExists(c.Storage.CacheDir); err != nil {
		return fmt.Errorf("validate storage.cache_dir: %w", err)
	}
	if c.Resize.MaxWidth <= 0 || c.Resize.MaxHeight <= 0 {
		return errInvalidGeometryLimit
	}
	if c.Resize.JPGQuality <= 0 || c.Resize.JPGQuality > 100 {
		return fmt.Errorf("resize.jpg_quality must be within 1-100, got %d", c.Resize.JPGQuality)
	}
	if c.Resize.WebPQuality < 0 || c.Resize.WebPQuality > 100 {
		return fmt.Errorf("resize.webp_quality must be within 0-100, got %d", c.Resize.WebPQuality)
	}
	if c.Resize.AVIFQuality < 0 || c.Resize.AVIFQuality > 100 {
		return fmt.Errorf("resize.avif_quality must be within 0-100, got %d", c.Resize.AVIFQuality)
	}
	if c.Resize.PNGCompression < 0 || c.Resize.PNGCompression > 9 {
		return fmt.Errorf("resize.png_compression must be within 0-9, got %d", c.Resize.PNGCompression)
	}
	return nil
}

// ApplyRewrites passes the input through rewrite rules until a match occurs.
func (c *Config) ApplyRewrites(input string) string {
	target := input
	for _, rule := range c.Rewrites {
		if output, ok := rule.Apply(target); ok {
			return output
		}
	}
	return target
}

func (c *Config) compile() error {
	for i := range c.Rewrites {
		if strings.TrimSpace(c.Rewrites[i].Pattern) == "" {
			return fmt.Errorf("rewrite rule %d has empty pattern", i)
		}
		re, err := regexp.Compile(c.Rewrites[i].Pattern)
		if err != nil {
			return fmt.Errorf("compile rewrite rule %d: %w", i, err)
		}
		c.Rewrites[i].re = re
	}
	return nil
}

func ensureDirExists(path string) error {
	sanitized := strings.TrimSpace(path)
	if sanitized == "" {
		return errors.New("path cannot be empty")
	}
	info, err := os.Stat(sanitized)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("path %s does not exist", sanitized)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("path %s is not a directory", sanitized)
	}
	return nil
}

var durationPattern = regexp.MustCompile(`(?i)^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

func parseFlexibleDuration(raw string) (time.Duration, error) {
	matches := durationPattern.FindStringSubmatch(raw)
	if matches == nil {
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	var total time.Duration
	if matches[1] != "" {
		days, err := strconv.Atoi(matches[1])
		if err != nil {
			return 0, fmt.Errorf("parse duration days %q: %w", matches[1], err)
		}
		total += time.Duration(days) * 24 * time.Hour
	}
	if matches[2] != "" {
		hours, err := time.ParseDuration(matches[2] + "h")
		if err != nil {
			return 0, err
		}
		total += hours
	}
	if matches[3] != "" {
		mins, err := time.ParseDuration(matches[3] + "m")
		if err != nil {
			return 0, err
		}
		total += mins
	}
	if matches[4] != "" {
		secs, err := time.ParseDuration(matches[4] + "s")
		if err != nil {
			return 0, err
		}
		total += secs
	}
	return total, nil
}

// ResolveOriginalPath resolves a request path against base dir ensuring no traversal.
func (c *Config) ResolvePaths(relative string) (string, string, error) {
	prepared := strings.TrimPrefix(relative, "/")
	prepared = filepath.ToSlash(prepared)
	prepared = c.ApplyRewrites(prepared)
	clean := filepath.Clean(prepared)
	if clean == "." {
		return "", "", errors.New("empty target path")
	}
	if strings.HasPrefix(clean, "../") || clean == ".." {
		return "", "", errors.New("path attempts to escape base directory")
	}
	full := filepath.Join(c.Storage.BaseDir, filepath.FromSlash(clean))
	return clean, full, nil
}

// ResolveOriginalPath resolves a request path against base dir ensuring no traversal.
func (c *Config) ResolveOriginalPath(relative string) (string, error) {
	_, full, err := c.ResolvePaths(relative)
	return full, err
}

// CachePath returns the computed cache path for requested geometry and asset.
func (c *Config) CachePath(width, height int, relative string) string {
	prefix := fmt.Sprintf("%dx%d", width, height)
	prepared := strings.TrimPrefix(relative, "/")
	clean := filepath.Clean(prepared)
	return filepath.Join(c.Storage.CacheDir, prefix, filepath.FromSlash(clean))
}
