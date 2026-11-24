// Command api serves the notification HTTP API.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
	"github.com/tarikdeveci/insider-notification-system/internal/config"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		// config errors name the variable, never its value.
		observability.NewLogger(os.Stdout, "info").Error(err.Error())
		return 1
	}
	log := observability.NewLogger(os.Stdout, cfg.LogLevel)

	shutdownTracing, err := observability.SetupTracing(context.Background(), "notification-api")
	if err != nil {
		log.Error("tracing setup failed", observability.ErrAttrs(err)...)
		return 1
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			log.Error("tracing shutdown failed", observability.ErrAttrs(err)...)
		}
	}()

	// Parse errors can echo the URL, so only the error type is logged.
	pgCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		log.Error("invalid DATABASE_URL", observability.ErrAttrs(err)...)
		return 1
	}
	pgCfg.MaxConns = int32(cfg.PGMaxConns)
	pool, err := pgxpool.NewWithConfig(context.Background(), pgCfg)
	if err != nil {
		log.Error("postgres pool init failed", observability.ErrAttrs(err)...)
		return 1
	}
	defer pool.Close()

	st := store.New(pool)
	reg := prometheus.NewRegistry()
	metrics := observability.NewAPIMetrics(reg)
	observability.RegisterQueueDepth(reg, st, log)

	handler := api.NewServer(api.Deps{
		Store:   st,
		DB:      pool,
		Logger:  log,
		Metrics: metrics,
	})
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second, // batch endpoint needs headroom
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("api listening", "addr", cfg.HTTPAddr)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", observability.ErrAttrs(err)...)
			return 1
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Error("shutdown failed", observability.ErrAttrs(err)...)
			return 1
		}
	}
	return 0
}
