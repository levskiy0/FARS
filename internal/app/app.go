package app

import (
	"log/slog"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

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
