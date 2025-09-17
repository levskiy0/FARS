package server

import (
	"context"
	"net/http"
	"time"

	"log/slog"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/httpapi"
)

// Module exposes fx providers for the HTTP server.
var Module = fx.Options(
	fx.Provide(NewEngine),
	fx.Invoke(RegisterLifecycle),
)

// Params bundles dependencies for HTTP lifecycle registration.
type Params struct {
	fx.In

	Lifecycle fx.Lifecycle
	Config    *config.Config
	Engine    *gin.Engine
	Cache     *cache.Manager
	Logger    *slog.Logger
}

// NewEngine constructs the gin engine with registered routes.
func NewEngine(cfg *config.Config, handler *httpapi.Handler) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	handler.Register(r)
	return r
}

// RegisterLifecycle wires the HTTP server into fx lifecycle.
func RegisterLifecycle(p Params) {
	srv := &http.Server{
		Addr:              p.Config.Server.Address(),
		Handler:           p.Engine,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	var cleanupCancel context.CancelFunc

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			p.Logger.Info("starting HTTP server", slog.String("addr", srv.Addr))
			cleanupCtx, cancel := context.WithCancel(context.Background())
			cleanupCancel = cancel
			p.Cache.StartCleanup(cleanupCtx)
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					p.Logger.Error("http server failure", slog.Any("error", err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			p.Logger.Info("stopping HTTP server")
			if cleanupCancel != nil {
				cleanupCancel()
			}
			return srv.Shutdown(ctx)
		},
	})
}
