package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"fars/internal/metrics"
)

// Keys under which the handler leaves labels for the metrics middleware. The
// middleware runs after the handler returns, when the local variables that
// knew the outcome are gone.
const (
	cacheOutcomeKey = "fars.cache"
	formatKey       = "fars.format"
)

// routeLabels maps gin's registered path templates onto a fixed label set. The
// request path itself must never become a label: it is attacker-controlled, and
// one crawler would mint a new time series per URL.
var routeLabels = map[string]string{
	"/resize/:geometry/*filepath": "resize",
	"/cache/invalidate":           "invalidate",
	"/cclear/*filepath":           "clear",
}

// ObserveRequests records one sample per request. metricsPath is excluded so a
// scrape does not appear in the numbers it is collecting.
func ObserveRequests(metricsPath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if metricsPath != "" && c.Request.URL.Path == metricsPath {
			c.Next()
			return
		}
		start := time.Now()
		c.Next()
		elapsed := time.Since(start)

		route, known := routeLabels[c.FullPath()]
		if !known {
			// Anything gin could not route: one series, not one per URL.
			route = "unknown"
		}
		cache := metrics.CacheNone
		if v, ok := c.Get(cacheOutcomeKey); ok {
			cache, _ = v.(string)
		}
		status := strconv.Itoa(c.Writer.Status())

		metrics.HTTPRequests.WithLabelValues(route, c.Request.Method, status, cache).Inc()
		metrics.HTTPDuration.WithLabelValues(route, cache).Observe(elapsed.Seconds())
		// Only successful responses: an error page is not image bytes, and
		// counting it under format="none" made the series mean two things.
		if size := c.Writer.Size(); size > 0 && c.Writer.Status() < http.StatusMultipleChoices {
			format := "none"
			if v, ok := c.Get(formatKey); ok {
				format, _ = v.(string)
			}
			metrics.HTTPResponseBytes.WithLabelValues(route, format).Add(float64(size))
		}
	}
}

func markCache(c *gin.Context, outcome string) { c.Set(cacheOutcomeKey, outcome) }

func markFormat(c *gin.Context, format string) { c.Set(formatKey, format) }

// errorReason turns a status code into a bounded label. The error text cannot
// be used: it embeds paths and geometries.
func errorReason(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media"
	default:
		return "internal"
	}
}
