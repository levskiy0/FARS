package app

import (
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"log/slog"
	"os"
	"runtime"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/httpapi"
	"fars/internal/locker"
	"fars/internal/metrics"
	"fars/internal/processor"
	"fars/internal/server"
)

// Build constructs an fx application configured with all dependencies.
func Build(cfg *config.Config) *fx.App {
	logger := newLogger(cfg)
	applyRuntimeTuning(logger, cfg)

	return fx.New(
		fx.WithLogger(func() fxevent.Logger {
			return fxevent.NopLogger
		}),
		fx.Supply(
			cfg,
			logger,
		),
		fx.Provide(
			cache.NewManager,
			processor.New,
			locker.New,
			httpapi.NewHandler,
		),
		server.Module,
	)
}

func newLogger(cfg *config.Config) *slog.Logger {
	logging := config.LoggingConfig{Level: "info", Format: "text"}
	if cfg != nil {
		logging = cfg.Logging
	}
	opts := &slog.HandlerOptions{Level: logging.LevelSlog()}
	// Text is the default because it is what the deployed log pipeline parses
	// today: promtail keys FARS lines off `level=INFO`. Switching a deployment
	// to json means changing that pipeline in the same breath.
	var handler slog.Handler = slog.NewTextHandler(os.Stdout, opts)
	if logging.JSON() {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

func applyRuntimeTuning(logger *slog.Logger, cfg *config.Config) {
	if cfg == nil {
		return
	}
	// Since Go 1.25 the runtime applies the cgroup CPU quota itself, so a
	// container limited to 6 CPUs on a 64-core host gets GOMAXPROCS=6 without
	// being told, and keeps tracking the limit if it changes. Configuring it
	// by hand replaces that and freezes the tracking, so the only thing to do
	// with an unset value is nothing.
	effective := runtime.GOMAXPROCS(0)
	if cfg.Runtime.GOMAXPROCS > 0 {
		effective = cfg.Runtime.GOMAXPROCS
		runtime.GOMAXPROCS(effective)
	}

	quota := detectCPUQuota()
	attrs := []any{"value", effective, "host_cpus", runtime.NumCPU()}
	if quota > 0 {
		attrs = append(attrs, "cpu_quota", quota)
	}
	switch {
	case quota > 0 && float64(effective) > quota:
		// The condition the pinned setting was meant to prevent and ended up
		// causing: more runnable threads than the quota can serve, so the
		// kernel stops the process for the rest of every scheduling period.
		logger.Warn("GOMAXPROCS is above the container CPU quota; the kernel will throttle", attrs...)
	case cfg.Runtime.GOMAXPROCS > 0:
		logger.Info("GOMAXPROCS pinned by configuration", attrs...)
	default:
		logger.Info("GOMAXPROCS derived from the CPU limit", attrs...)
	}
	metrics.GOMAXPROCS.Set(float64(effective))
	metrics.CPUQuota.Set(quota)

	// libvips counts the host's CPUs, not the cgroup quota, so left alone it
	// starts a thread pool sized for the machine inside a container that may
	// only run a fraction of it. It is always set explicitly for that reason;
	// the default of 1 keeps parallelism where it is measurable — across
	// requests — instead of inside one operation.
	concurrency := cfg.Runtime.VIPSConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	configureVipsConcurrency(concurrency)
	metrics.VIPSConcurrency.Set(float64(concurrency))
	logger.Info("libvips concurrency", "value", concurrency)
}
