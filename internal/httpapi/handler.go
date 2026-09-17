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
	"runtime"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"github.com/gin-gonic/gin"

	"fars/internal/cache"
	"fars/internal/config"
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
	logger    *slog.Logger
	// resizeSem bounds the number of concurrent processor.Resize calls so a
	// burst of requests cannot pile up unbounded libvips/canvas allocations.
	resizeSem chan struct{}
}

// NewHandler constructs the HTTP handler.
func NewHandler(cfg *config.Config, cache *cache.Manager, processor *processor.Processor, logger *slog.Logger) *Handler {
	concurrency := cfg.Runtime.ResizeConcurrency
	if concurrency <= 0 {
		// One in-flight resize per schedulable CPU. Deliberately NOT
		// runtime.vips_concurrency: that knob sizes libvips' thread pool
		// *inside* a single operation, and deployments set it to 1 or 2 on
		// purpose, which would throttle the whole service to as many
		// concurrent requests.
		concurrency = runtime.GOMAXPROCS(0)
		if concurrency <= 0 {
			concurrency = runtime.NumCPU()
		}
	}
	return &Handler{
		cfg:       cfg,
		cache:     cache,
		processor: processor,
		logger:    logger.With("component", "handler"),
		resizeSem: make(chan struct{}, concurrency),
	}
}

// Register attaches routes to gin engine.
func (h *Handler) Register(r *gin.Engine) {
	r.GET("/resize/:geometry/*filepath", h.handleResize)
	r.HEAD("/resize/:geometry/*filepath", h.handleResize)
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
	if strings.ContainsRune(relative, 0) {
		// A NUL byte makes os.Stat fail with EINVAL rather than
		// os.ErrNotExist, which would otherwise fall through to a 500.
		// Treated as malformed input (400), not merely "not found".
		h.respondError(c, http.StatusBadRequest, errors.New("path contains a NUL byte"))
		return
	}
	rawExt := filepath.Ext(relative)
	ext := strings.ToLower(rawExt)
	format, ok := extensionToFormat[ext]
	if !ok {
		h.respondError(c, http.StatusUnsupportedMediaType, fmt.Errorf("unsupported extension %q", ext))
		return
	}
	// Every access to an original goes through this root: it resolves each
	// path component itself and refuses anything that leaves base_dir,
	// including via a symlink. filepath.Join + os.Stat cannot do that — they
	// follow a link straight out of the directory.
	root, err := os.OpenRoot(h.cfg.Storage.BaseDir)
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, fmt.Errorf("open base dir: %w", err))
		return
	}
	defer root.Close()

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
		info, statErr := root.Stat(rootRelative(cleanCandidate))
		if statErr != nil {
			if isSourceUnreachable(statErr) {
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
		if !isRegularFile(info) {
			// A directory (or other non-regular entry) is not an image;
			// treated the same as "not found" rather than surfacing whatever
			// os.ReadFile/bimg would fail with further down.
			lastClean = cleanCandidate
			if i == len(candidates)-1 {
				h.respondError(c, http.StatusNotFound, fmt.Errorf("original not found: %s", cleanCandidate))
				return
			}
			continue
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
			h.logAccess(c, width, height, cacheRel, originalInfo.ModTime(), true, time.Since(start))
			return
		}
	}

	releaseOriginal := h.cache.LockOriginal(originalRel)
	defer releaseOriginal()

	refreshedInfo, statErr := root.Stat(rootRelative(originalRel))
	if statErr != nil {
		if isSourceUnreachable(statErr) {
			h.respondError(c, http.StatusNotFound, fmt.Errorf("original not found: %s", originalRel))
			return
		}
		h.respondError(c, http.StatusInternalServerError, fmt.Errorf("stat original: %w", statErr))
		return
	}
	if !isRegularFile(refreshedInfo) {
		h.respondError(c, http.StatusNotFound, fmt.Errorf("original not found: %s", originalRel))
		return
	}
	originalInfo = refreshedInfo

	release := h.cache.LockCache(cachePath)
	defer release()
	if h.cache.IsFresh(cachePath, originalInfo) {
		if served := h.tryServeFromCache(c, cachePath, format, originalInfo); served {
			h.logAccess(c, width, height, cacheRel, originalInfo.ModTime(), true, time.Since(start))
			return
		}
	}

	source, err := readSource(root, rootRelative(originalRel))
	if err != nil {
		h.respondError(c, http.StatusInternalServerError, fmt.Errorf("read original: %w", err))
		return
	}

	if c.Request.Context().Err() != nil {
		// Client is already gone; don't spend a resize slot or any CPU on it.
		return
	}

	// Admission control: wait for a free slot rather than piling up unbounded
	// concurrent libvips/canvas work. A queued request only gives up if the
	// client disconnects while waiting.
	select {
	case h.resizeSem <- struct{}{}:
	case <-c.Request.Context().Done():
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
		MaxWidth:       h.cfg.Resize.MaxWidth,
		MaxHeight:      h.cfg.Resize.MaxHeight,
	})
	// The slot covers the resize itself and nothing else: held across the
	// response write, one slow client would keep another resize out.
	<-h.resizeSem
	if err != nil {
		switch {
		case errors.Is(err, processor.ErrDimensionsTooLarge), errors.Is(err, processor.ErrDegenerateGeometry):
			h.respondError(c, http.StatusBadRequest, err)
		case errors.Is(err, processor.ErrUnsupportedSource):
			h.respondError(c, http.StatusUnsupportedMediaType, err)
		default:
			h.respondError(c, http.StatusInternalServerError, err)
		}
		return
	}

	// Truncated to the second, the precision Last-Modified is serialised at, so a
	// client echoing back the exact value it was given compares equal instead of
	// losing the sub-second remainder and always revalidating. Same rule as
	// net/http's ServeContent.
	modTime := originalInfo.ModTime().UTC().Truncate(time.Second)

	// Serve the generated content FIRST with strong caching headers — unless
	// the client hung up while the resize was running, in which case there is
	// nobody left to write the response to.
	if c.Request.Context().Err() != nil {
		h.logger.Warn("client disconnected, skipping response", "path", cachePath)
	} else {
		h.respondWithPayload(c, payload, format, modTime)
	}

	// THEN save to cache; if it fails, log an error but do not fail the request.
	// The bytes exist either way, so they are stored even for a client that is
	// already gone: dropping them would make the next visitor pay for the very
	// same resize again.
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

	h.logAccess(c, width, height, cacheRel, originalInfo.ModTime(), false, time.Since(start))
}

// respondWithPayload writes a freshly rendered variant, honouring the
// conditional-request headers the client sent.
func (h *Handler) respondWithPayload(c *gin.Context, payload []byte, format processor.Format, modTime time.Time) {
	etag := buildContentETag(payload)
	notModified := func() {
		c.Header("Cache-Control", cacheControlImmutable)
		c.Header("ETag", etag)
		c.Header("Last-Modified", modTime.Format(http.TimeFormat))
		c.Status(http.StatusNotModified)
	}

	if matchETag(c.GetHeader("If-None-Match"), etag) {
		notModified()
		return
	}
	if ifModifiedSince := c.GetHeader("If-Modified-Since"); c.GetHeader("If-None-Match") == "" && ifModifiedSince != "" {
		if t, err := http.ParseTime(ifModifiedSince); err == nil && !modTime.After(t.UTC()) {
			notModified()
			return
		}
	}

	c.Header("Content-Type", formatContentType[format])
	c.Header("Cache-Control", cacheControlImmutable)
	c.Header("ETag", etag)
	c.Header("Last-Modified", modTime.Format(http.TimeFormat))
	c.Header("Content-Length", strconv.Itoa(len(payload)))
	c.Data(http.StatusOK, formatContentType[format], payload)
}

type invalidateRequest struct {
	Paths []string `json:"paths" binding:"required,min=1,max=1000"`
}

func (h *Handler) authorizeInvalidation(c *gin.Context) bool {
	// Compare fixed-size digests rather than the raw tokens: a length check
	// in front of subtle.ConstantTimeCompare would itself leak the expected
	// token's length to a timing attacker.
	expectedSum := sha256.Sum256([]byte("Bearer " + h.cfg.Cache.InvalidationToken))
	providedSum := sha256.Sum256([]byte(c.GetHeader("Authorization")))
	if subtle.ConstantTimeCompare(expectedSum[:], providedSum[:]) != 1 {
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
	// Track what actually got deleted so a mid-batch failure can still tell
	// the caller which paths are already gone — otherwise a retry of the
	// whole batch would be the only option, and it wouldn't be idempotent.
	// One batch call, so the cache geometries are listed once rather than once
	// per path. Counts come back in input order and stop at the first failure.
	counts, err := h.cache.InvalidateOriginals(c.Request.Context(), invalidated)
	succeeded := invalidated[:len(counts)]
	for _, count := range counts {
		removed += count
	}
	if err != nil {
		failed := ""
		if len(counts) < len(invalidated) {
			failed = invalidated[len(counts)]
		}
		h.logger.Error("manual cache invalidation failed", slog.String("path", failed), slog.Any("error", err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":            "cache invalidation failed",
			"path":             failed,
			"invalidated":      succeeded,
			"variants_removed": removed,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"invalidated": succeeded, "variants_removed": removed})
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
	// 0x0 is not an error: the PrestaShop module emits it whenever it cannot
	// work out a size, and it means "fit inside resize.max_width x
	// resize.max_height, keeping the aspect ratio, without upscaling".
	if width > 0 && width > h.cfg.Resize.MaxWidth {
		return fmt.Errorf("width %d exceeds limit %d", width, h.cfg.Resize.MaxWidth)
	}
	if height > 0 && height > h.cfg.Resize.MaxHeight {
		return fmt.Errorf("height %d exceeds limit %d", height, h.cfg.Resize.MaxHeight)
	}
	return nil
}

// rootRelative turns a cleaned request path into a name an os.Root accepts:
// slash-separated, never absolute.
func rootRelative(clean string) string {
	return filepath.FromSlash(strings.TrimPrefix(filepath.ToSlash(clean), "/"))
}

// readSource reads an original through the base-dir root, so a symlinked
// entry cannot smuggle in a file from outside it.
func readSource(root *os.Root, name string) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// isSourceUnreachable reports whether an os.Root lookup failed in a way that
// means "there is no original here": it does not exist, or the path leaves
// base_dir through a symlink or a .. segment. Both answer 404 — a link out of
// the originals directory is not a servable image. os keeps the escape
// sentinel unexported, so it is matched by message; a miss only downgrades
// the response to a 500, it never grants access.
func isSourceUnreachable(err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Err != nil && pathErr.Err.Error() == "path escapes from parent" {
		return true
	}
	return false
}

// isRegularFile reports whether info describes a plain file, rejecting
// directories, sockets, devices, etc. that stat successfully but are not
// something we can read as an image.
func isRegularFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular()
}

func (h *Handler) tryServeFromCache(c *gin.Context, cachePath string, format processor.Format, originalInfo os.FileInfo) bool {
	_, file, err := h.cache.ServeFileStats(cachePath)
	if err != nil {
		return false
	}
	defer file.Close()

	payload, err := io.ReadAll(file)
	if err != nil {
		return false
	}
	etag := buildContentETag(payload)
	// Use the original's mtime, not the cache file's own, so If-Modified-Since
	// revalidation is consistent between a cache hit and a freshly rendered
	// response for the same original. Truncated to the second for the same
	// reason as in handleResize.
	modTime := originalInfo.ModTime().UTC().Truncate(time.Second)

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
	// The service name/version stays out of the client-facing body; it's
	// still available to operators via the startup log.
	body := fmt.Sprintf("<html><head><title>%s</title></head>\n<body>\n<center><h1>%s</h1></center>\n<hr><center>%s</center>\n</body></html> ", title, title, "FARS")
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

// logAccess records a successfully served request. Failures are logged by
// respondError instead, so this only ever reports success.
func (h *Handler) logAccess(c *gin.Context, width, height int, rel string, originalMod time.Time, cached bool, dur time.Duration) {
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
	h.logger.Info("served image", attrs...)
}
