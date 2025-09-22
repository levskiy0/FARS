// internal/config/config.go
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf"
	yamlparser "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/providers/structs"
	"github.com/mitchellh/mapstructure"
	"gopkg.in/yaml.v3"

	"fars/pkg/configutil"
)

var (
	errEmptyConfigPath      = errors.New("config path is empty")
	errInvalidGeometryLimit = errors.New("resize max dimensions must be positive")
	envPathLookup           = buildEnvPathLookup()
	envShortcutLookup       = map[string]string{
		"HOST":             "server.host",
		"PORT":             "server.port",
		"IMAGES_BASE_DIR":  "storage.base_dir",
		"CACHE_DIR":        "storage.cache_dir",
		"MAX_WIDTH":        "resize.max_width",
		"MAX_HEIGHT":       "resize.max_height",
		"JPG_QUALITY":      "resize.jpg_quality",
		"WEBP_QUALITY":     "resize.webp_quality",
		"AVIF_QUALITY":     "resize.avif_quality",
		"PNG_COMPRESSION":  "resize.png_compression",
		"AVIF_SPEED":       "resize.avif_speed",
		"GOMAXPROCS":       "runtime.gomaxprocs",
		"VIPS_CONCURRENCY": "runtime.vips_concurrency",
		"TTL":              "cache.ttl",
		"CLEANUP_INTERVAL": "cache.cleanup_interval",
	}
)

// Config represents the full service configuration loaded from YAML.
type Config struct {
	Server   ServerConfig  `yaml:"server"`
	Storage  StorageConfig `yaml:"storage"`
	Resize   ResizeConfig  `yaml:"resize"`
	Cache    CacheConfig   `yaml:"cache"`
	Runtime  RuntimeConfig `yaml:"runtime"`
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
	AVIFSpeed      int `yaml:"avif_speed"`
}

// RuntimeConfig controls Go scheduler and libvips concurrency.
type RuntimeConfig struct {
	GOMAXPROCS      int `yaml:"gomaxprocs"`
	VIPSConcurrency int `yaml:"vips_concurrency"`
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

// ByteSize represents a capacity parsed from human readable strings (e.g. 300mb).
type ByteSize struct {
	Bytes int64
}

// defaultConfig returns sane defaults when no YAML is provided.
func defaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Storage: StorageConfig{
			BaseDir:  "/data/base",
			CacheDir: "/data/cache",
		},
		Resize: ResizeConfig{
			MaxWidth:       2000,
			MaxHeight:      2000,
			JPGQuality:     80,
			WebPQuality:    75,
			AVIFQuality:    75,
			PNGCompression: 6,
			AVIFSpeed:      8,
		},
		Cache: CacheConfig{
			TTL:             Duration{30 * 24 * time.Hour}, // 30d
			CleanupInterval: Duration{24 * time.Hour},      // 24h
		},
		Runtime: RuntimeConfig{},
	}
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return nil
	}
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string, got kind %d", value.Kind)
	}
	return d.parseFromString(value.Value)
}

// UnmarshalText allows decoding durations from koanf/env providers.
func (d *Duration) UnmarshalText(text []byte) error {
	return d.parseFromString(string(text))
}

func (d *Duration) parseFromString(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.EqualFold(trimmed, "null") {
		d.Duration = 0
		return nil
	}
	dur, err := configutil.ParseFlexibleDuration(trimmed)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

// UnmarshalYAML implements yaml.Unmarshaler for byte sizes.
func (b *ByteSize) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return nil
	}
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("byte size must be a scalar, got kind %d", value.Kind)
	}
	return b.parseFromString(value.Value)
}

// UnmarshalText allows decoding byte sizes from koanf/env providers.
func (b *ByteSize) UnmarshalText(text []byte) error {
	return b.parseFromString(string(text))
}

func (b *ByteSize) parseFromString(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.EqualFold(trimmed, "null") {
		b.Bytes = 0
		return nil
	}
	size, err := configutil.ParseByteSize(trimmed)
	if err != nil {
		return err
	}
	b.Bytes = size
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
	return loadConfig(path, nil, false)
}

// LoadReader decodes configuration from an arbitrary reader.
func LoadReader(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return loadConfig("", data, false)
}

// LoadFromEnvOrFile loads configuration from YAML if path is provided;
// otherwise starts from defaultConfig(). Env vars (if present) override both.
func LoadFromEnvOrFile(path string) (*Config, error) {
	return loadConfig(path, nil, true)
}

func loadConfig(path string, raw []byte, allowMissing bool) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(structs.Provider(*defaultConfig(), "yaml"), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}
	sourcePath := strings.TrimSpace(path)
	switch {
	case len(raw) > 0:
		if err := k.Load(rawbytes.Provider(raw), yamlparser.Parser()); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	case sourcePath != "":
		if err := k.Load(file.Provider(sourcePath), yamlparser.Parser()); err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}
	case !allowMissing:
		return nil, errEmptyConfigPath
	}
	if err := loadEnvVars(k); err != nil {
		return nil, err
	}
	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		Tag: "yaml",
		DecoderConfig: &mapstructure.DecoderConfig{
			TagName:          "yaml",
			WeaklyTypedInput: true,
			Result:           &cfg,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.TextUnmarshallerHookFunc(),
			),
		},
	}); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if err := cfg.compile(); err != nil {
		return nil, err
	}
	return &cfg, cfg.Validate()
}

func loadEnvVars(k *koanf.Koanf) error {
	for _, prefix := range []string{"FARS_", ""} {
		if err := k.Load(env.Provider(prefix, ".", canonicalEnvKey), nil); err != nil {
			return fmt.Errorf("load env: %w", err)
		}
	}
	return nil
}

func canonicalEnvKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "FARS_") {
		trimmed = strings.TrimPrefix(trimmed, "FARS_")
	}
	if strings.Contains(trimmed, "__") {
		lower := strings.ToLower(trimmed)
		return strings.ReplaceAll(lower, "__", ".")
	}
	upper := strings.ToUpper(trimmed)
	if mapped, ok := envShortcutLookup[upper]; ok {
		return mapped
	}
	if mapped, ok := envPathLookup[upper]; ok {
		return mapped
	}
	return ""
}

func buildEnvPathLookup() map[string]string {
	result := make(map[string]string)
	var walk func(reflect.Type, []string)
	walk = func(t reflect.Type, path []string) {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name := field.Tag.Get("yaml")
			if name == "" || name == "-" {
				name = strings.ToLower(field.Name)
			} else {
				name = strings.Split(name, ",")[0]
			}
			if name == "" || name == "-" {
				continue
			}
			current := append(append([]string{}, path...), name)
			typ := field.Type
			base := typ
			for base.Kind() == reflect.Pointer {
				base = base.Elem()
			}
			switch base.Kind() {
			case reflect.Struct:
				if base != reflect.TypeOf(Duration{}) && base != reflect.TypeOf(ByteSize{}) && base != reflect.TypeOf(time.Time{}) {
					walk(base, current)
					continue
				}
			case reflect.Slice, reflect.Map, reflect.Array:
				continue
			}
			key := strings.ToUpper(strings.Join(current, "_"))
			result[key] = strings.Join(current, ".")
		}
	}
	walk(reflect.TypeOf(Config{}), nil)
	return result
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
	if c.Resize.AVIFSpeed < 0 || c.Resize.AVIFSpeed > 8 {
		return fmt.Errorf("resize.avif_speed must be within 0-8, got %d", c.Resize.AVIFSpeed)
	}
	if c.Runtime.GOMAXPROCS < 0 {
		return fmt.Errorf("runtime.gomaxprocs must be >= 0, got %d", c.Runtime.GOMAXPROCS)
	}
	if c.Runtime.VIPSConcurrency < 0 {
		return fmt.Errorf("runtime.vips_concurrency must be >= 0, got %d", c.Runtime.VIPSConcurrency)
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
			// Create the directory tree if it doesn't exist
			if mkErr := os.MkdirAll(sanitized, 0o755); mkErr != nil {
				return fmt.Errorf("create dir %s: %w", sanitized, mkErr)
			}
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("path %s is not a directory", sanitized)
	}
	return nil
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
	prefix := formatGeometryPrefix(width, height)
	prepared := strings.TrimPrefix(relative, "/")
	clean := filepath.Clean(prepared)
	return filepath.Join(c.Storage.CacheDir, prefix, filepath.FromSlash(clean))
}

func formatGeometryPrefix(width, height int) string {
	var (
		w string
		h string
	)
	if width > 0 {
		w = strconv.Itoa(width)
	}
	if height > 0 {
		h = strconv.Itoa(height)
	}
	if w == "" && h == "" {
		return "0x0"
	}
	return w + "x" + h
}
