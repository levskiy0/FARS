package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"log/slog"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"fars/internal/cache"
	"fars/internal/config"
	"fars/internal/httpapi"
	"fars/internal/metrics"
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
	// Instrumentation goes outside Recovery, not inside it. A panic unwinds
	// past every middleware between it and the recover(), so an inner
	// ObserveRequests would skip its own bookkeeping and the 500 the client
	// received would appear nowhere in the metrics.
	r.Use(httpapi.ObserveRequests(sharedMetricsPath(cfg)))
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
	if path := sharedMetricsPath(cfg); path != "" {
		r.GET(path, gin.WrapH(metrics.Handler()))
	}
	return r
}

// sharedMetricsPath returns the path the exposition endpoint occupies on the
// main listener, or "" when metrics are disabled or moved to their own port.
func sharedMetricsPath(cfg *config.Config) string {
	if cfg == nil || !cfg.Metrics.Enabled || strings.TrimSpace(cfg.Metrics.Listen) != "" {
		return ""
	}
	return strings.TrimSpace(cfg.Metrics.Path)
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
	metricsSrv := newMetricsServer(p.Config)

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
			if metricsSrv != nil {
				p.Logger.Info("serving metrics", slog.String("addr", metricsSrv.Addr), slog.String("path", p.Config.Metrics.Path))
				go func() {
					// A metrics port that cannot bind must not take the image
					// service down with it; it is reported and the resize
					// endpoint keeps serving.
					if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						p.Logger.Error("metrics server failure", slog.Any("error", err))
					}
				}()
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			p.Logger.Info("stopping HTTP server")
			shutdownErr := srv.Shutdown(ctx)
			if metricsSrv != nil {
				shutdownErr = errors.Join(shutdownErr, metricsSrv.Shutdown(ctx))
			}
			if backgroundCancel != nil {
				backgroundCancel()
			}
			return errors.Join(shutdownErr, p.Cache.WaitBackground(ctx))
		},
	})
}

// newMetricsServer builds the dedicated exposition listener, or nil when the
// endpoint shares the API listener (or is switched off entirely).
func newMetricsServer(cfg *config.Config) *http.Server {
	if cfg == nil || !cfg.Metrics.Enabled {
		return nil
	}
	addr := strings.TrimSpace(cfg.Metrics.Listen)
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle(strings.TrimSpace(cfg.Metrics.Path), metrics.Handler())
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
}
