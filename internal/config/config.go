package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	yamlparser "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
	"gopkg.in/yaml.v3"

	"fars/pkg/configutil"
)

var (
	errEmptyConfigPath      = errors.New("config path is empty")
	errInvalidGeometryLimit = errors.New("resize max dimensions must be positive")
	envPathLookup           = buildEnvPathLookup()
	envShortcutLookup       = map[string]string{
		"HOST":                     "server.host",
		"PORT":                     "server.port",
		"IMAGES_BASE_DIR":          "storage.base_dir",
		"CACHE_DIR":                "storage.cache_dir",
		"MAX_WIDTH":                "resize.max_width",
		"MAX_HEIGHT":               "resize.max_height",
		"JPG_QUALITY":              "resize.jpg_quality",
		"WEBP_QUALITY":             "resize.webp_quality",
		"AVIF_QUALITY":             "resize.avif_quality",
		"PNG_COMPRESSION":          "resize.png_compression",
		"AVIF_SPEED":               "resize.avif_speed",
		"GOMAXPROCS":               "runtime.gomaxprocs",
		"VIPS_CONCURRENCY":         "runtime.vips_concurrency",
		"RESIZE_CONCURRENCY":       "runtime.resize_concurrency",
		"TTL":                      "cache.ttl",
		"CLEANUP_INTERVAL":         "cache.cleanup_interval",
		"CHECK_ORIGINALS_INTERVAL": "cache.check_originals_interval",
		"INVALIDATION_TOKEN":       "cache.invalidation_token",
		"MAX_CACHE_SIZE":           "cache.max_size",
	}
	// legacyEnvShortcutLookup is the allowlist for unprefixed (legacy)
	// environment variables, kept for backward compatibility with the
	// shipped Dockerfile and existing deployments. It is a strict subset of
	// envShortcutLookup: generic names such as HOST are intentionally
	// excluded here so an ambient env var can't silently repoint the
	// service. FARS_HOST (and the rest of the FARS_ namespace) remains the
	// preferred, unrestricted way to configure the service.
	legacyEnvShortcutLookup = buildLegacyEnvShortcutLookup()
)

func buildLegacyEnvShortcutLookup() map[string]string {
	legacy := make(map[string]string, len(envShortcutLookup))
	for key, path := range envShortcutLookup {
		if key == "HOST" {
			continue
		}
		legacy[key] = path
	}
	return legacy
}

// Config represents the full service configuration loaded from YAML.
type Config struct {
	Server   ServerConfig  `yaml:"server"`
	Storage  StorageConfig `yaml:"storage"`
	Resize   ResizeConfig  `yaml:"resize"`
	Cache    CacheConfig   `yaml:"cache"`
	Runtime  RuntimeConfig `yaml:"runtime"`
	Metrics  MetricsConfig `yaml:"metrics"`
	Logging  LoggingConfig `yaml:"logging"`
	Rewrites []RewriteRule `yaml:"rewrites"`
}

// MetricsConfig describes the Prometheus exposition endpoint.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
	// Listen, when set, moves the endpoint to its own host:port instead of
	// sharing the API listener. That is the deployment where the image
	// serves the public internet through a proxy and the metrics port is
	// published only on a private interface.
	Listen string `yaml:"listen"`
}

// LoggingConfig describes log output.
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// Access toggles the per-request line. Metrics cover rates and latency
	// without it; on a busy catalogue it is the bulk of the log volume.
	Access bool `yaml:"access"`
}

// LevelSlog maps the configured level onto slog's.
func (l LoggingConfig) LevelSlog() slog.Level {
	switch strings.ToLower(strings.TrimSpace(l.Level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// JSON reports whether logs should be emitted as JSON objects.
func (l LoggingConfig) JSON() bool {
	return strings.EqualFold(strings.TrimSpace(l.Format), "json")
}

// ServerConfig describes HTTP server binding parameters.
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// TrustedProxies lists the IPs/CIDRs of the reverse proxies in front of
	// FARS whose X-Forwarded-For header may be believed. Empty trusts none,
	// so the access log records the immediate peer.
	TrustedProxies []string `yaml:"trusted_proxies"`
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
	// ResizeConcurrency bounds how many resizes may run at once. 0 means one
	// per schedulable CPU (GOMAXPROCS). It is deliberately separate from
	// VIPSConcurrency, which sizes the thread pool inside a single libvips
	// operation and is routinely set to 1.
	ResizeConcurrency int `yaml:"resize_concurrency"`
}

// CacheConfig stores cache retention settings.
type CacheConfig struct {
	TTL                     Duration `yaml:"ttl"`
	CleanupInterval         Duration `yaml:"cleanup_interval"`
	CheckOriginalsInterval  Duration `yaml:"check_originals_interval"`
	CheckOriginalsWorkers   int      `yaml:"check_originals_workers"`
	InvalidationLockTimeout Duration `yaml:"invalidation_lock_timeout"`
	InvalidationToken       string   `yaml:"invalidation_token"`
	MaxSize                 ByteSize `yaml:"max_size"`
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
			// FARS sits behind a reverse proxy on a private network. Believing
			// X-Forwarded-For from private peers is what makes remote_ip the
			// visitor rather than the proxy; a client on the public internet
			// cannot reach this list, because its source address is public.
			// Narrow it to the proxy's own address if FARS shares a private
			// network with anything untrusted.
			TrustedProxies: []string{"127.0.0.1/32", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},
		},
		Storage: StorageConfig{
			BaseDir:  "/data/base",
			CacheDir: "/data/cache",
		},
		// The encoder settings are the ones the Citimarine storefront runs
		// on, arrived at by looking at what the crawlers actually pull and
		// what the files cost: below these the artefacts start showing on
		// product photography, above them the bytes buy nothing visible.
		Resize: ResizeConfig{
			MaxWidth:       2000,
			MaxHeight:      2000,
			JPGQuality:     77,
			WebPQuality:    65,
			AVIFQuality:    50,
			PNGCompression: 6,
			AVIFSpeed:      6,
		},
		Cache: CacheConfig{
			TTL:                     Duration{10 * 24 * time.Hour}, // 10d
			CleanupInterval:         Duration{12 * time.Hour},
			CheckOriginalsInterval:  Duration{5 * time.Minute},
			CheckOriginalsWorkers:   4,
			InvalidationLockTimeout: Duration{100 * time.Millisecond},
			// A cap, because the alternative default is "grow until the
			// volume is full": every distinct geometry writes another file and
			// nothing but ttl removes them. 50gb is the same order as the
			// originals tree it serves; a deployment with a bigger disk should
			// raise it, and "0" restores unbounded growth deliberately.
			MaxSize: ByteSize{Bytes: 50 << 30},
		},
		// gomaxprocs and vips_concurrency stay 0: the Go runtime reads the
		// cgroup CPU quota itself, and libvips is pinned to a single thread
		// per operation by applyRuntimeTuning. Pinning either by hand is how
		// a container ends up oversubscribing its own quota.
		Runtime: RuntimeConfig{},
		Metrics: MetricsConfig{
			Enabled: true,
			Path:    "/metrics",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
			Access: true,
		},
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

// numericSecondsToDurationHookFunc lets a bare YAML/env integer or float
// (e.g. `ttl: 0` or `cleanup_interval: 3600`) decode into a Duration field
// as a count of seconds, alongside the existing string forms ("30d", "24h").
// It leaves string sources untouched so mapstructure.TextUnmarshallerHookFunc
// still handles those.
func numericSecondsToDurationHookFunc() mapstructure.DecodeHookFuncType {
	return func(from reflect.Type, to reflect.Type, data any) (any, error) {
		if to != reflect.TypeOf(Duration{}) {
			return data, nil
		}
		switch from.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			seconds := reflect.ValueOf(data).Int()
			return Duration{time.Duration(seconds) * time.Second}, nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			seconds := reflect.ValueOf(data).Uint()
			return Duration{time.Duration(seconds) * time.Second}, nil
		case reflect.Float32, reflect.Float64:
			seconds := reflect.ValueOf(data).Float()
			return Duration{time.Duration(seconds * float64(time.Second))}, nil
		default:
			return data, nil
		}
	}
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
				numericSecondsToDurationHookFunc(),
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

// loadEnvVars applies environment overrides in two passes. The legacy
// unprefixed pass is loaded first and only recognizes an explicit allowlist
// of names; the FARS_-prefixed pass is loaded last so it always wins over
// both YAML and any ambient unprefixed variable of the same shortcut name.
func loadEnvVars(k *koanf.Koanf) error {
	legacyOpt := env.Opt{Prefix: "", TransformFunc: func(key, value string) (string, any) {
		return legacyEnvKey(key), value
	}}
	if err := k.Load(env.Provider(".", legacyOpt), nil); err != nil {
		return fmt.Errorf("load env: %w", err)
	}
	scopedOpt := env.Opt{Prefix: "FARS_", TransformFunc: func(key, value string) (string, any) {
		return canonicalEnvKey(key), value
	}}
	if err := k.Load(env.Provider(".", scopedOpt), nil); err != nil {
		return fmt.Errorf("load env: %w", err)
	}
	return nil
}

// legacyEnvKey maps an unprefixed (legacy) environment variable name to its
// config path. It only recognizes the explicit legacyEnvShortcutLookup
// allowlist: no nested "__" path syntax and no generic per-field names, so
// an unrelated ambient variable can't repoint the service. Anything already
// carrying the FARS_ prefix is left to the scoped pass.
func legacyEnvKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" || strings.HasPrefix(trimmed, "FARS_") {
		return ""
	}
	upper := strings.ToUpper(trimmed)
	if mapped, ok := legacyEnvShortcutLookup[upper]; ok {
		return mapped
	}
	return ""
}

// canonicalEnvKey maps a FARS_-prefixed environment variable name to its
// config path, supporting the full "__" nested-path syntax plus the
// shortcut and generic path lookups.
func canonicalEnvKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "FARS_")
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
	if c.Cache.CheckOriginalsWorkers < 0 || c.Cache.CheckOriginalsWorkers > 64 {
		return errors.New("cache.check_originals_workers must be between 0 and 64")
	}
	if c.Cache.InvalidationLockTimeout.Duration < 0 {
		return errors.New("cache.invalidation_lock_timeout must be non-negative")
	}
	if c.Cache.MaxSize.Bytes < 0 {
		return errors.New("cache.max_size must be non-negative")
	}
	if strings.TrimSpace(c.Server.Host) == "" {
		return errors.New("server.host must be set")
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535, got %d", c.Server.Port)
	}
	if err := c.validateMetrics(); err != nil {
		return err
	}
	if err := c.validateLogging(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Storage.BaseDir) == "" {
		return errors.New("storage.base_dir must be set")
	}
	if strings.TrimSpace(c.Storage.CacheDir) == "" {
		return errors.New("storage.cache_dir must be set")
	}
	if err := checkDirExists(c.Storage.BaseDir); err != nil {
		return fmt.Errorf("validate storage.base_dir: %w", err)
	}
	if err := validateStorageContainment(c.Storage.BaseDir, c.Storage.CacheDir); err != nil {
		return err
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
	if c.Runtime.ResizeConcurrency < 0 {
		return fmt.Errorf("runtime.resize_concurrency must be >= 0, got %d", c.Runtime.ResizeConcurrency)
	}
	if err := validateTrustedProxies(c.Server.TrustedProxies); err != nil {
		return err
	}
	return nil
}

// validateTrustedProxies checks every entry is an IP address or a CIDR block,
// the two forms gin's SetTrustedProxies accepts. A typo here would otherwise
// only surface at startup as an opaque engine error.
func validateTrustedProxies(entries []string) error {
	for i, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			return fmt.Errorf("server.trusted_proxies[%d] must not be empty", i)
		}
		if strings.Contains(trimmed, "/") {
			if _, _, err := net.ParseCIDR(trimmed); err != nil {
				return fmt.Errorf("server.trusted_proxies[%d] %q is not a valid CIDR: %w", i, entry, err)
			}
			continue
		}
		if net.ParseIP(trimmed) == nil {
			return fmt.Errorf("server.trusted_proxies[%d] %q is not a valid IP address or CIDR", i, entry)
		}
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

// checkDirExists verifies that path already exists and is a directory. It
// never creates anything: storage.base_dir holds the originals, and silently
// creating it would let a broken bind mount masquerade as an empty, healthy
// base directory (and have its "orphaned" cache pruned as a result).
func checkDirExists(path string) error {
	sanitized := strings.TrimSpace(path)
	if sanitized == "" {
		return errors.New("path cannot be empty")
	}
	info, err := os.Stat(sanitized)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("directory %s does not exist", sanitized)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("path %s is not a directory", sanitized)
	}
	return nil
}

// validateStorageContainment rejects storage.base_dir/storage.cache_dir
// combinations where one contains the other, they resolve to the same
// directory, or either resolves to the filesystem root. base_dir is
// expected to already exist (checked by checkDirExists); cache_dir may not
// exist yet, so it is resolved against its nearest existing ancestor.
func validateStorageContainment(baseDir, cacheDir string) error {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return fmt.Errorf("resolve storage.base_dir %q: %w", baseDir, err)
	}
	absCache, err := filepath.Abs(cacheDir)
	if err != nil {
		return fmt.Errorf("resolve storage.cache_dir %q: %w", cacheDir, err)
	}
	resolvedBase, err := filepath.EvalSymlinks(absBase)
	if err != nil {
		return fmt.Errorf("resolve storage.base_dir %q: %w", baseDir, err)
	}
	resolvedCache, err := resolveExistingAncestor(absCache)
	if err != nil {
		return fmt.Errorf("resolve storage.cache_dir %q: %w", cacheDir, err)
	}
	root := string(filepath.Separator)
	if resolvedBase == root {
		return fmt.Errorf("storage.base_dir %q resolves to the filesystem root, which is not allowed", baseDir)
	}
	if resolvedCache == root {
		return fmt.Errorf("storage.cache_dir %q resolves to the filesystem root, which is not allowed", cacheDir)
	}
	if resolvedBase == resolvedCache {
		return fmt.Errorf("storage.cache_dir %q and storage.base_dir %q must not be the same directory", cacheDir, baseDir)
	}
	if isSubPath(resolvedBase, resolvedCache) {
		return fmt.Errorf("storage.cache_dir %q must not be inside storage.base_dir %q", cacheDir, baseDir)
	}
	if isSubPath(resolvedCache, resolvedBase) {
		return fmt.Errorf("storage.base_dir %q must not be inside storage.cache_dir %q", baseDir, cacheDir)
	}
	return nil
}

// resolveExistingAncestor resolves symlinks along path, walking up to the
// nearest ancestor that actually exists (path itself may not exist yet),
// then rejoins the non-existent trailing components onto the resolved base.
func resolveExistingAncestor(path string) (string, error) {
	current := path
	var pending []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil {
				return "", evalErr
			}
			for i := len(pending) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, pending[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the root without finding an existing ancestor.
			return path, nil
		}
		pending = append(pending, filepath.Base(current))
		current = parent
	}
}

// isSubPath reports whether child is strictly inside parent (both already
// absolute and symlink-resolved). Equal paths are not considered contained.
func isSubPath(parent, child string) bool {
	if parent == child {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
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

// validateMetrics keeps the exposition endpoint from silently ending up
// somewhere it cannot be scraped, or on top of the resize routes.
func (c *Config) validateMetrics() error {
	if !c.Metrics.Enabled {
		return nil
	}
	path := strings.TrimSpace(c.Metrics.Path)
	if path == "" {
		return errors.New("metrics.path must be set when metrics are enabled")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("metrics.path must start with a slash, got %q", path)
	}
	// gin panics when a literal route collides with an existing wildcard one,
	// which would turn a typo into a crash loop instead of a config error.
	for _, reserved := range []string{"/resize", "/cache", "/cclear"} {
		if path == reserved || strings.HasPrefix(path, reserved+"/") {
			return fmt.Errorf("metrics.path %q collides with the %s routes", path, reserved)
		}
	}
	listen := strings.TrimSpace(c.Metrics.Listen)
	if listen == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("metrics.listen must be host:port, got %q", listen)
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 || value > 65535 {
		return fmt.Errorf("metrics.listen port must be between 1 and 65535, got %q", port)
	}
	if host != "" && net.ParseIP(host) == nil {
		return fmt.Errorf("metrics.listen host must be an IP or empty, got %q", host)
	}
	if listen == c.Server.Address() {
		return errors.New("metrics.listen must differ from the server address; leave it empty to share the API listener")
	}
	return nil
}

func (c *Config) validateLogging() error {
	switch strings.ToLower(strings.TrimSpace(c.Logging.Level)) {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("logging.level must be one of debug, info, warn, error, got %q", c.Logging.Level)
	}
	switch strings.ToLower(strings.TrimSpace(c.Logging.Format)) {
	case "", "text", "json":
		return nil
	default:
		return fmt.Errorf("logging.format must be text or json, got %q", c.Logging.Format)
	}
}
