package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"github.com/gin-gonic/gin"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/locker"
	"fars/internal/processor"
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
	locks     *locker.KeyedLocker
	logger    *slog.Logger
}

// NewHandler constructs the HTTP handler.
func NewHandler(cfg *config.Config, cache *cache.Manager, processor *processor.Processor, locks *locker.KeyedLocker, logger *slog.Logger) *Handler {
	return &Handler{
		cfg:       cfg,
		cache:     cache,
		processor: processor,
		locks:     locks,
		logger:    logger.With("component", "handler"),
	}
}

// Register attaches routes to gin engine.
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/resize/:geometry/*filepath", h.handleResize)
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

	release := h.locks.Lock(cachePath)
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
		PNGCompression: h.cfg.Resize.PNGCompression,
		EnsureOpaque:   ensureOpaque,
	})
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, err)
		return
	}

	if err := h.cache.Write(cachePath, payload); err != nil {
		h.respondError(c, http.StatusInternalServerError, fmt.Errorf("store cache: %w", err))
		return
	}

	if served := h.tryServeFromCache(c, cachePath, format, originalInfo); served {
		h.logAccess(c, width, height, cacheRel, originalInfo.ModTime(), false, time.Since(start), nil)
		return
	}

	h.respondError(c, http.StatusInternalServerError, errors.New("unable to open cached file"))
}

type sourceCandidate struct {
	relative    string
	cacheSuffix string
}

func buildSourceCandidates(relative, rawExt string) []sourceCandidate {
	candidates := []sourceCandidate{{relative: relative}}
	if rawExt == "" {
		return candidates
	}
	base := strings.TrimSuffix(relative, rawExt)
	if base == relative {
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
	if ifModifiedSince != "" {
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
	body := fmt.Sprintf("<html><head><title>%s</title></head>\n<body>\n<center><h1>%s</h1></center>\n<hr><center>FARS</center>\n</body></html> ", title, title)
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
