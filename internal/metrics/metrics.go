// Package metrics holds the Prometheus instrumentation FARS exposes.
//
// The collectors are package-level on purpose. Prometheus collectors are
// process-wide state — a counter is meaningless scoped to one object — and
// threading a registry through every constructor would add a parameter to
// code paths that only want to add 1 to a number. They are registered on a
// private registry rather than prometheus.DefaultRegisterer so the exposition
// contains what this package declares and nothing a dependency happens to
// register on the global default.
package metrics

import (
	"net/http"
	"runtime/debug"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"fars/internal/version"
)

const namespace = "fars"

// Cache-outcome label values for a request.
const (
	CacheHit  = "hit"  // served from cache_dir without resizing
	CacheMiss = "miss" // the variant had to be produced
	CacheNone = "none" // the request never got as far as looking
)

// Reasons a cache entry was removed. These are the only values
// fars_cache_removals_total carries.
const (
	ReasonTTL          = "ttl"
	ReasonOrphan       = "orphan"
	ReasonOutdated     = "outdated"
	ReasonSize         = "size"
	ReasonInvalidation = "invalidation"
	ReasonStaleTemp    = "stale_temp"
)

var registry = prometheus.NewRegistry()

// indexStats is installed by the cache manager and read at scrape time. The
// index changes on every published and invalidated variant; a gauge updated
// at those call sites would be one forgotten call site away from lying, and
// it did lie — originals_tracked stayed at whatever discovery last saw.
var indexStats atomic.Pointer[IndexStatsFunc]

// IndexStatsFunc reports the live state of the originals index.
type IndexStatsFunc func() (tracked int, ready bool)

// SetIndexStats installs the callback behind fars_originals_tracked and
// fars_originals_index_ready. It must be cheap: it runs inside a scrape.
func SetIndexStats(fn IndexStatsFunc) {
	if fn == nil {
		indexStats.Store(nil)
		return
	}
	indexStats.Store(&fn)
}

func readIndexStats() (int, bool) {
	fn := indexStats.Load()
	if fn == nil {
		return 0, false
	}
	return (*fn)()
}

var (
	// BuildInfo is the usual join target: 1, labelled with what is running.
	BuildInfo = newGaugeVec(prometheus.GaugeOpts{
		Name: "build_info",
		Help: "Always 1, labelled with the running FARS build.",
	}, "version", "go_version")

	// HTTPRequests counts finished requests. No path label: the path is
	// attacker-controlled and would be unbounded cardinality.
	HTTPRequests = newCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Requests served, by route, method, status class and cache outcome.",
	}, "route", "method", "code", "cache")

	HTTPDuration = newHistogramVec(prometheus.HistogramOpts{
		Name: "http_request_duration_seconds",
		Help: "Wall time from the first line of the handler to the last byte written.",
		// A cache hit is sub-millisecond; a cold AVIF of a large original is
		// seconds. The buckets have to span both or the p99 is a guess.
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, "route", "cache")

	HTTPResponseBytes = newCounterVec(prometheus.CounterOpts{
		Name: "http_response_bytes_total",
		Help: "Image bytes written to clients, by route and output format.",
	}, "route", "format")

	// ResizeDuration measures processor.Resize alone — decode, resize and
	// encode — with neither the queue wait nor the response write in it.
	ResizeDuration = newHistogramVec(prometheus.HistogramOpts{
		Name:    "resize_duration_seconds",
		Help:    "Time spent inside libvips producing one variant.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, "format")

	// ResizeQueueWait is how long a request waited for an admission slot.
	// Non-zero here with idle CPU means resize_concurrency is the limit;
	// non-zero with pegged CPU means the box is.
	ResizeQueueWait = newHistogram(prometheus.HistogramOpts{
		Name:    "resize_queue_wait_seconds",
		Help:    "Time a request waited for a resize slot before work started.",
		Buckets: []float64{0.0001, 0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})

	ResizeInFlight = newGauge(prometheus.GaugeOpts{
		Name: "resize_in_flight",
		Help: "Resizes running right now.",
	})

	// ResizeSlots is the denominator for ResizeInFlight: saturation is the
	// ratio of the two, which is not derivable from the gauge alone.
	ResizeSlots = newGauge(prometheus.GaugeOpts{
		Name: "resize_slots",
		Help: "Configured maximum of concurrent resizes.",
	})

	ResizeSourceBytes = newCounter(prometheus.CounterOpts{
		Name: "resize_source_bytes_total",
		Help: "Original bytes read from storage for resizing.",
	})

	Errors = newCounterVec(prometheus.CounterOpts{
		Name: "request_errors_total",
		Help: "Requests that ended in an error response, by cause.",
	}, "reason")

	CacheWrites = newCounterVec(prometheus.CounterOpts{
		Name: "cache_writes_total",
		Help: "Attempts to publish a variant into the cache, by result.",
	}, "result")

	CacheRemovals = newCounterVec(prometheus.CounterOpts{
		Name: "cache_removals_total",
		Help: "Cache entries removed, by reason.",
	}, "reason")

	CacheRemovedBytes = newCounterVec(prometheus.CounterOpts{
		Name: "cache_removed_bytes_total",
		Help: "Bytes freed from the cache, by reason.",
	}, "reason")

	CacheSizeBytes = newGauge(prometheus.GaugeOpts{
		Name: "cache_size_bytes",
		Help: "Cache size as measured by the last sweep, not a live figure.",
	})

	CacheFiles = newGauge(prometheus.GaugeOpts{
		Name: "cache_files",
		Help: "Cached variants counted by the last sweep.",
	})

	CacheSizeLimitBytes = newGauge(prometheus.GaugeOpts{
		Name: "cache_size_limit_bytes",
		Help: "Configured cache.max_size; 0 when no cap is set.",
	})

	CacheSweepDuration = newHistogram(prometheus.HistogramOpts{
		Name:    "cache_sweep_duration_seconds",
		Help:    "Duration of a full cleanup sweep over cache_dir.",
		Buckets: []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900},
	})

	// CacheSweepLast is a timestamp so alerting can say "the sweep has not
	// finished for two intervals", which a counter cannot express.
	CacheSweepLast = newGauge(prometheus.GaugeOpts{
		Name: "cache_sweep_last_success_timestamp_seconds",
		Help: "Unix time of the last cleanup sweep that finished without error.",
	})

	OriginalsChanged = newCounter(prometheus.CounterOpts{
		Name: "originals_changed_total",
		Help: "Originals seen to change, each invalidating its variants.",
	})

	OriginalsCheckErrors = newCounter(prometheus.CounterOpts{
		Name: "originals_check_errors_total",
		Help: "Monitor checks that failed and were deferred.",
	})

	Invalidations = newCounterVec(prometheus.CounterOpts{
		Name: "invalidations_total",
		Help: "Manual invalidation requests, by result.",
	}, "result")
)

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "originals_tracked",
			Help:      "Originals the monitor currently watches.",
		}, func() float64 {
			tracked, _ := readIndexStats()
			return float64(tracked)
		}),
		// The one to alert on: while it is 0 a changed original is not
		// noticed, and invalidation falls back to a scan.
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "originals_index_ready",
			Help:      "1 once cache discovery has completed, 0 while it has not.",
		}, func() float64 {
			_, ready := readIndexStats()
			if ready {
				return 1
			}
			return 0
		}),
	)
	goVersion := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok && info.GoVersion != "" {
		goVersion = info.GoVersion
	}
	BuildInfo.WithLabelValues(version.Identifier(), goVersion).Set(1)
	initSeries()
}

// initSeries materialises the label combinations that are known up front, so
// they read 0 from the first scrape instead of being absent. rate() over a
// series that appears only when the first error happens reports nothing at the
// moment it starts mattering, and a dashboard panel shows "No data" rather
// than a flat line. Only bounded families are listed: request labels include
// the status code and are left to appear as they occur.
func initSeries() {
	for _, reason := range []string{"bad_request", "unauthorized", "not_found", "unsupported_media", "internal"} {
		Errors.WithLabelValues(reason)
	}
	for _, reason := range []string{ReasonTTL, ReasonOrphan, ReasonOutdated, ReasonSize, ReasonInvalidation, ReasonStaleTemp} {
		CacheRemovals.WithLabelValues(reason)
		CacheRemovedBytes.WithLabelValues(reason)
	}
	for _, result := range []string{"ok", "error"} {
		CacheWrites.WithLabelValues(result)
		Invalidations.WithLabelValues(result)
	}
	for _, format := range []string{"jpeg", "png", "webp", "avif"} {
		ResizeDuration.WithLabelValues(format)
	}
}

// Registry returns the registry every FARS collector lives on.
func Registry() *prometheus.Registry { return registry }

// Handler serves the exposition format.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		// A broken collector should show up as a scrape error in Prometheus,
		// not as a 500 that hides every other metric in the same response.
		ErrorHandling: promhttp.ContinueOnError,
		Registry:      registry,
	})
}

func newCounter(opts prometheus.CounterOpts) prometheus.Counter {
	opts.Namespace = namespace
	c := prometheus.NewCounter(opts)
	registry.MustRegister(c)
	return c
}

func newCounterVec(opts prometheus.CounterOpts, labels ...string) *prometheus.CounterVec {
	opts.Namespace = namespace
	c := prometheus.NewCounterVec(opts, labels)
	registry.MustRegister(c)
	return c
}

func newGauge(opts prometheus.GaugeOpts) prometheus.Gauge {
	opts.Namespace = namespace
	g := prometheus.NewGauge(opts)
	registry.MustRegister(g)
	return g
}

func newGaugeVec(opts prometheus.GaugeOpts, labels ...string) *prometheus.GaugeVec {
	opts.Namespace = namespace
	g := prometheus.NewGaugeVec(opts, labels)
	registry.MustRegister(g)
	return g
}

func newHistogram(opts prometheus.HistogramOpts) prometheus.Histogram {
	opts.Namespace = namespace
	h := prometheus.NewHistogram(opts)
	registry.MustRegister(h)
	return h
}

func newHistogramVec(opts prometheus.HistogramOpts, labels ...string) *prometheus.HistogramVec {
	opts.Namespace = namespace
	h := prometheus.NewHistogramVec(opts, labels)
	registry.MustRegister(h)
	return h
}
