package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"log/slog"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/httpapi"
	"fars/internal/version"
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
	// gin defaults to trusting every proxy, which lets any client spoof
	// X-Forwarded-For and land it verbatim in c.ClientIP() / the access log's
	// remote_ip field. Only the proxies named in server.trusted_proxies are
	// believed; an empty list (the default) trusts nobody, and remote_ip is
	// then whoever actually opened the connection.
	if err := r.SetTrustedProxies(cfg.Server.TrustedProxies); err != nil {
		// The entries were already validated by config.Validate, so this only
		// fires if the two ever disagree.
		panic(fmt.Errorf("server.trusted_proxies: %w", err))
	}
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

	var backgroundCancel context.CancelFunc

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			p.Logger.Info("starting HTTP server", slog.String("addr", srv.Addr), slog.String("version", version.Identifier()))
			backgroundCtx, cancel := context.WithCancel(context.Background())
			backgroundCancel = cancel
			p.Cache.StartBackground(backgroundCtx)
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					p.Logger.Error("http server failure", slog.Any("error", err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			p.Logger.Info("stopping HTTP server")
			shutdownErr := srv.Shutdown(ctx)
			if backgroundCancel != nil {
				backgroundCancel()
			}
			return errors.Join(shutdownErr, p.Cache.WaitBackground(ctx))
		},
	})
}
