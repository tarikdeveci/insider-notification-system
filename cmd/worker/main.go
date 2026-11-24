// Command worker claims queued notifications and delivers them to the provider.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/tarikdeveci/insider-notification-system/internal/config"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/provider"
	"github.com/tarikdeveci/insider-notification-system/internal/ratelimit"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	"github.com/tarikdeveci/insider-notification-system/internal/worker"
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

	shutdownTracing, err := observability.SetupTracing(context.Background(), "notification-worker")
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

	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Error("invalid REDIS_URL", observability.ErrAttrs(err)...)
		return 1
	}
	opt.PoolSize = cfg.RedisPoolSize
	// Context deadlines (the limiter's remaining wait budget) must bound socket I/O.
	opt.ContextTimeoutEnabled = true
	rdb := redis.NewClient(opt)
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Error("close redis client", observability.ErrAttrs(err)...)
		}
	}()

	// The client error never contains the URL (it carries the webhook id).
	reg := prometheus.NewRegistry()
	metrics := observability.NewWorkerMetrics(reg)
	prov, err := provider.NewClient(cfg.ProviderURL, provider.WithLogger(log), provider.WithUnparsedCounter(metrics.UnparsedBody()))
	if err != nil {
		log.Error("invalid PROVIDER_URL: " + err.Error())
		return 1
	}

	st := store.New(pool)
	lim := ratelimit.New(rdb, ratelimit.WithMaxWait(cfg.RateLimitMaxWait))
	engine := worker.New(st, prov, lim, worker.Settings{
		Concurrency:      cfg.WorkerConcurrency,
		ClaimBatch:       cfg.ClaimBatch,
		ShutdownTimeout:  cfg.ShutdownTimeout,
		ReaperInterval:   cfg.ReaperInterval,
		MaxDeliveryAge:   cfg.MaxDeliveryAge,
		Policy:           worker.Policy{Base: cfg.RetryBaseDelay, Cap: cfg.RetryMaxDelay, MaxAge: cfg.MaxDeliveryAge},
		BreakerThreshold: cfg.BreakerThreshold,
		BreakerOpenFor:   cfg.BreakerOpenFor,
	}, worker.WithLogger(log), worker.WithObserver(metrics))

	srv := &http.Server{
		Addr:              cfg.WorkerHTTPAddr,
		Handler:           healthMux(pool.Ping, func(ctx context.Context) error { return rdb.Ping(ctx).Err() }, metrics.Handler()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpErr := make(chan error, 1)
	go func() { httpErr <- srv.ListenAndServe() }()
	log.Info("worker started", "http_addr", cfg.WorkerHTTPAddr)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = engine.Run(ctx)
	}()

	code := 0
	select {
	case err := <-httpErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", observability.ErrAttrs(err)...)
			code = 1
		}
		stop() // stop the engine too
	case <-ctx.Done():
		log.Info("shutting down")
	}
	wg.Wait() // Run returns after in-flight sends finish or SHUTDOWN_TIMEOUT passes

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("health server shutdown failed", observability.ErrAttrs(err)...)
		code = 1
	}
	return code
}

// healthMux serves /metrics, /healthz (always 200) and /readyz (200 when both pings pass, else 503 problem JSON).
func healthMux(pingPG, pingRedis func(context.Context) error, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		check := func(f func(context.Context) error) string {
			if f(ctx) != nil {
				return "fail"
			}
			return "ok"
		}
		pg, rd := check(pingPG), check(pingRedis)
		if pg == "ok" && rd == "ok" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "checks": map[string]string{"postgres": pg, "redis": rd}})
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "about:blank", "title": http.StatusText(http.StatusServiceUnavailable), "status": http.StatusServiceUnavailable,
			"detail": "dependency check failed", "checks": map[string]string{"postgres": pg, "redis": rd},
		})
	})
	return mux
}
