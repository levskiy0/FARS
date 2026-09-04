package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"github.com/gin-gonic/gin"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/processor"
	"fars/internal/version"
)

var (
	extensionToFormat = map[string]processor.Format{
		".jpg":  processor.FormatJPEG,
		".jpeg": processor.FormatJPEG,
		".png":  processor.FormatPNG,
		".webp": processor.FormatWEBP,
		".avif": processor.FormatAVIF,
	}
	formatContentType = map[processor.Format]string{
		processor.FormatJPEG: "image/jpeg",
		processor.FormatPNG:  "image/png",
		processor.FormatWEBP: "image/webp",
		processor.FormatAVIF: "image/avif",
	}
)

// Handler serves /resize endpoints.
type Handler struct {
	cfg       *config.Config
	cache     *cache.Manager
	processor *processor.Processor
	logger    *slog.Logger
}

// NewHandler constructs the HTTP handler.
func NewHandler(cfg *config.Config, cache *cache.Manager, processor *processor.Processor, logger *slog.Logger) *Handler {
	return &Handler{
		cfg:       cfg,
		cache:     cache,
		processor: processor,
		logger:    logger.With("component", "handler"),
	}
}

// Register attaches routes to gin engine.
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/resize/:geometry/*filepath", h.handleResize)
	if strings.TrimSpace(h.cfg.Cache.InvalidationToken) != "" {
		r.POST("/cache/invalidate", h.handleInvalidate)
		r.POST("/cclear/*filepath", h.handleClear)
	}
}

func (h *Handler) handleResize(c *gin.Context) {
	start := time.Now()
	geometry := c.Param("geometry")
	width, height, err := parseGeometry(geometry)
	if err != nil {
		h.respondError(c, http.StatusBadRequest, err)
		return
	}
	if err := h.validateDimensions(width, height); err != nil {
		h.respondError(c, http.StatusBadRequest, err)
		return
	}

	relative := c.Param("filepath")
	if relative == "" {
		h.respondError(c, http.StatusBadRequest, errors.New("path is required"))
		return
	}

	if strings.Contains(relative, "%20") {
		relative = strings.ReplaceAll(relative, "%20", " ")
	}
	relative = strings.TrimPrefix(relative, "/")
	relative = filepath.ToSlash(relative)
	if strings.TrimSpace(relative) == "" {
		h.respondError(c, http.StatusBadRequest, errors.New("path is required"))
		return
	}
	rawExt := filepath.Ext(relative)
	ext := strings.ToLower(rawExt)
	format, ok := extensionToFormat[ext]
	if !ok {
		h.respondError(c, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported extension %q", ext))
		return
	}
	candidates := buildSourceCandidates(relative, rawExt)
	var (
		cacheRel     string
		originalRel  string
		originalPath string
		originalInfo os.FileInfo
		lastClean    string
		ensureOpaque bool
	)
	for i, cand := range candidates {
		cleanCandidate, candidatePath, err := h.cfg.ResolvePaths(cand.relative)
		if err != nil {
			h.respondError(c, http.StatusBadRequest, err)
			return
		}
		info, statErr := os.Stat(candidatePath)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				lastClean = cleanCandidate
				if i == len(candidates)-1 {
					h.respondError(c, http.StatusNotFound, fmt.Errorf("original not found: %s", cleanCandidate))
					return
				}
				continue
			}
			h.respondError(c, http.StatusInternalServerError, fmt.Errorf("stat original: %w", statErr))
			return
		}
		originalRel = cleanCandidate
		originalPath = candidatePath
		originalInfo = info
		cacheRel = cleanCandidate
		if cand.cacheSuffix != "" {
			cacheRel = cleanCandidate + cand.cacheSuffix
		}
		ensureOpaque = hasJPEGExtension(cleanCandidate)
		break
	}
	if originalInfo == nil {
		missing := lastClean
		if missing == "" {
			missing = relative
		}
		h.respondError(c, http.StatusNotFound, fmt.Errorf("original not found: %s", missing))
		return
	}

	cachePath := h.cfg.CachePath(width, height, cacheRel)
	if h.cache.IsFresh(cachePath, originalInfo) {
		if served := h.tryServeFromCache(c, cachePath, format, originalInfo); served {
			h.logAccess(c, width, height, cacheRel, originalInfo.ModTime(), true, time.Since(start), nil)
			return
		}
	}

	releaseOriginal := h.cache.LockOriginal(originalRel)
	defer releaseOriginal()

	refreshedInfo, statErr := os.Stat(originalPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			h.respondError(c, http.StatusNotFound, fmt.Errorf("original not found: %s", originalRel))
			return
		}
		h.respondError(c, http.StatusInternalServerError, fmt.Errorf("stat original: %w", statErr))
		return
	}
	originalInfo = refreshedInfo

	release := h.cache.LockCache(cachePath)
	defer release()
	if h.cache.IsFresh(cachePath, originalInfo) {
		if served := h.tryServeFromCache(c, cachePath, format, originalInfo); served {
			h.logAccess(c, width, height, cacheRel, originalInfo.ModTime(), true, time.Since(start), nil)
			return
		}
	}

	source, err := os.ReadFile(originalPath)
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, fmt.Errorf("read original: %w", err))
		return
	}

	payload, err := h.processor.Resize(source, processor.Options{
		Width:          width,
		Height:         height,
		Format:         format,
		JPEGQuality:    h.cfg.Resize.JPGQuality,
		WebPQuality:    h.cfg.Resize.WebPQuality,
		AVIFQuality:    h.cfg.Resize.AVIFQuality,
		AVIFSpeed:      h.cfg.Resize.AVIFSpeed,
		PNGCompression: h.cfg.Resize.PNGCompression,
		EnsureOpaque:   ensureOpaque,
	})
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, err)
		return
	}

	// Serve generated content FIRST with strong caching headers.
	etag := buildContentETag(payload)
	modTime := originalInfo.ModTime().UTC()

	if matchETag(c.GetHeader("If-None-Match"), etag) {
		c.Header("Cache-Control", cacheControlImmutable)
		c.Header("ETag", etag)
		c.Header("Last-Modified", modTime.Format(http.TimeFormat))
		c.Status(http.StatusNotModified)
	} else if ifModifiedSince := c.GetHeader("If-Modified-Since"); c.GetHeader("If-None-Match") == "" && ifModifiedSince != "" {
		if t, err := http.ParseTime(ifModifiedSince); err == nil && !modTime.After(t.UTC()) {
			c.Header("Cache-Control", cacheControlImmutable)
			c.Header("ETag", etag)
			c.Header("Last-Modified", modTime.Format(http.TimeFormat))
			c.Status(http.StatusNotModified)
		} else {
			c.Header("Content-Type", formatContentType[format])
			c.Header("Cache-Control", cacheControlImmutable)
			c.Header("ETag", etag)
			c.Header("Last-Modified", modTime.Format(http.TimeFormat))
			c.Header("Content-Length", strconv.Itoa(len(payload)))
			c.Data(http.StatusOK, formatContentType[format], payload)
		}
	} else {
		c.Header("Content-Type", formatContentType[format])
		c.Header("Cache-Control", cacheControlImmutable)
		c.Header("ETag", etag)
		c.Header("Last-Modified", modTime.Format(http.TimeFormat))
		c.Header("Content-Length", strconv.Itoa(len(payload)))
		c.Data(http.StatusOK, formatContentType[format], payload)
	}

	// THEN try to save to cache; if it fails, log an error but do not fail the request.
	if err := h.cache.Write(cachePath, originalRel, originalInfo, payload); err != nil {
		h.logger.Error("cache store failed",
			"path", cachePath,
			"error", err,
			"origin_mtime", originalInfo.ModTime().UTC(),
			"width", width,
			"height", height,
			"source_path", originalPath,
		)
	}

	h.logAccess(c, width, height, cacheRel, originalInfo.ModTime(), false, time.Since(start), nil)
}

type invalidateRequest struct {
	Paths []string `json:"paths" binding:"required,min=1,max=1000"`
}

func (h *Handler) authorizeInvalidation(c *gin.Context) bool {
	expected := []byte("Bearer " + h.cfg.Cache.InvalidationToken)
	provided := []byte(c.GetHeader("Authorization"))
	if len(expected) != len(provided) || subtle.ConstantTimeCompare(expected, provided) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return false
	}
	return true
}

// handleClear accepts a URL-encoded original path; Gin has already decoded it.
func (h *Handler) handleClear(c *gin.Context) {
	if !h.authorizeInvalidation(c) {
		return
	}
	h.invalidatePaths(c, []string{strings.TrimPrefix(c.Param("filepath"), "/")})
}

func (h *Handler) handleInvalidate(c *gin.Context) {
	if !h.authorizeInvalidation(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var request invalidateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	h.invalidatePaths(c, request.Paths)
}

func (h *Handler) invalidatePaths(c *gin.Context, paths []string) {
	removed := 0
	invalidated := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean, _, err := h.cfg.ResolvePaths(path)
		// Match the manager's validation before deleting ANY path in the batch.
		// ResolvePaths permits a leading slash and may also rewrite a path.
		if err == nil && (strings.ContainsRune(path, 0) || strings.ContainsRune(clean, 0) || filepath.IsAbs(clean)) {
			err = errors.New("invalid original path")
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "path": path})
			return
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		invalidated = append(invalidated, clean)
	}
	for _, clean := range invalidated {
		count, err := h.cache.InvalidateOriginal(c.Request.Context(), clean)
		if err != nil {
			h.logger.Error("manual cache invalidation failed", slog.String("path", clean), slog.Any("error", err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "cache invalidation failed", "path": clean})
			return
		}
		removed += count
	}
	c.JSON(http.StatusOK, gin.H{"invalidated": invalidated, "variants_removed": removed})
}

type sourceCandidate struct {
	relative    string
	cacheSuffix string
}

// psVariantRe matches PrestaShop SEO image URLs of the form
// `{id}-{image_type}/{slug}.{ext}` where the leading segment is a numeric
// image id followed by an image-type suffix. The variant portion is dropped
// so FARS always resolves to the bare original `{id}/{slug}.{ext}` and
// resizes on the fly — variants no longer need to be pre-generated by PS.
var psVariantRe = regexp.MustCompile(`^(\d+)-[\w-]+(/.+)$`)

// stripPSVariant removes a trailing `-{image_type}` suffix from the first
// path segment when it looks like a PrestaShop product image URL. Non
// PrestaShop paths are returned unchanged.
func stripPSVariant(relative string) string {
	if m := psVariantRe.FindStringSubmatch(relative); m != nil {
		return m[1] + m[2]
	}
	return relative
}

func buildSourceCandidates(relative, rawExt string) []sourceCandidate {
	normalized := stripPSVariant(relative)
	candidates := []sourceCandidate{{relative: normalized}}
	if rawExt == "" {
		return candidates
	}
	base := strings.TrimSuffix(normalized, rawExt)
	if base == normalized {
		return candidates
	}
	baseExt := strings.ToLower(filepath.Ext(base))
	if _, ok := extensionToFormat[baseExt]; ok {
		candidates = append(candidates, sourceCandidate{
			relative:    base,
			cacheSuffix: strings.ToLower(rawExt),
		})
	}
	return candidates
}

func hasJPEGExtension(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".jpg" || ext == ".jpeg"
}

func (h *Handler) validateDimensions(width, height int) error {
	if width < 0 || height < 0 {
		return errors.New("dimensions must be non-negative")
	}
	if width > 0 && width > h.cfg.Resize.MaxWidth {
		return fmt.Errorf("width %d exceeds limit %d", width, h.cfg.Resize.MaxWidth)
	}
	if height > 0 && height > h.cfg.Resize.MaxHeight {
		return fmt.Errorf("height %d exceeds limit %d", height, h.cfg.Resize.MaxHeight)
	}
	return nil
}

func (h *Handler) tryServeFromCache(c *gin.Context, cachePath string, format processor.Format, originalInfo os.FileInfo) bool {
	info, file, err := h.cache.ServeFileStats(cachePath)
	if err != nil {
		return false
	}
	defer file.Close()

	payload, err := io.ReadAll(file)
	if err != nil {
		return false
	}
	etag := buildContentETag(payload)
	modTime := info.ModTime().UTC()

	if matchETag(c.GetHeader("If-None-Match"), etag) {
		c.Header("Cache-Control", cacheControlImmutable)
		c.Header("ETag", etag)
		c.Header("Last-Modified", modTime.Format(http.TimeFormat))
		c.Status(http.StatusNotModified)
		return true
	}

	ifModifiedSince := c.GetHeader("If-Modified-Since")
	if c.GetHeader("If-None-Match") == "" && ifModifiedSince != "" {
		if t, err := http.ParseTime(ifModifiedSince); err == nil {
			if !modTime.After(t.UTC()) {
				c.Header("Cache-Control", cacheControlImmutable)
				c.Header("ETag", etag)
				c.Header("Last-Modified", modTime.Format(http.TimeFormat))
				c.Status(http.StatusNotModified)
				return true
			}
		}
	}

	c.Header("Content-Type", formatContentType[format])
	c.Header("Cache-Control", cacheControlImmutable)
	c.Header("ETag", etag)
	c.Header("Last-Modified", modTime.Format(http.TimeFormat))
	c.Header("Content-Length", strconv.Itoa(len(payload)))
	c.Data(http.StatusOK, formatContentType[format], payload)
	return true
}

func (h *Handler) respondError(c *gin.Context, code int, err error) {
	h.logger.Error("request error",
		slog.Any("error", err),
		slog.Int("status", code),
		slog.String("geometry", c.Param("geometry")),
		slog.String("path", c.Param("filepath")))
	title := fmt.Sprintf("%d %s", code, http.StatusText(code))
	body := fmt.Sprintf("<html><head><title>%s</title></head>\n<body>\n<center><h1>%s</h1></center>\n<hr><center>%s</center>\n</body></html> ", title, title, version.Identifier())
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.String(code, body)
	c.Abort()
}

func parseGeometry(geometry string) (int, int, error) {
	parts := strings.SplitN(geometry, "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid geometry %q", geometry)
	}
	width, err := parseDimension(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid width: %w", err)
	}
	height, err := parseDimension(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid height: %w", err)
	}
	return width, height, nil
}

func parseDimension(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return value, nil
}

const cacheControlImmutable = "public, max-age=31536000, immutable, s-maxage=31536000"

func buildContentETag(payload []byte) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("\"%s\"", hex.EncodeToString(sum[:]))
}

func matchETag(header string, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}

func (h *Handler) logAccess(c *gin.Context, width, height int, rel string, originalMod time.Time, cached bool, dur time.Duration, err error) {
	attrs := []any{
		"remote_ip", c.ClientIP(),
		"width", width,
		"height", height,
		"path", rel,
		"cached", cached,
		"duration_ms", dur.Milliseconds(),
	}
	if !originalMod.IsZero() {
		attrs = append(attrs, "origin_mtime", originalMod.UTC())
	}
	if err != nil {
		h.logger.Error("request failed", append(attrs, "error", err)...)
		return
	}
	h.logger.Info("served image", attrs...)
}
