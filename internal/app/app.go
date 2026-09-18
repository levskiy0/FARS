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
	if cfg.Runtime.GOMAXPROCS > 0 {
		prev := runtime.GOMAXPROCS(cfg.Runtime.GOMAXPROCS)
		logger.Info("set GOMAXPROCS", "value", cfg.Runtime.GOMAXPROCS, "previous", prev)
	}
	if cfg.Runtime.VIPSConcurrency > 0 {
		configureVipsConcurrency(cfg.Runtime.VIPSConcurrency)
		logger.Info("set libvips concurrency", "value", cfg.Runtime.VIPSConcurrency)
	}
}
