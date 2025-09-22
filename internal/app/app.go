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
	logger := newLogger()
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

func newLogger() *slog.Logger {
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
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
