// Command mockprovider runs the configurable mock notification provider.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/mockprovider"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
)

func main() {
	os.Exit(run())
}

// loadConfig reads the MOCK_* variables; errors name the variable, never its value.
func loadConfig(getenv func(string) string) (string, mockprovider.Config, error) {
	cfg := mockprovider.Config{FailureStatus: http.StatusServiceUnavailable, RetryAfterS: 1}
	addr := ":9090"
	if v := getenv("MOCK_ADDR"); v != "" {
		addr = v
	}
	if v := getenv("MOCK_FAILURE_RATE"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return "", cfg, errors.New("invalid MOCK_FAILURE_RATE")
		}
		cfg.FailureRate = f
	}
	if v := getenv("MOCK_FAILURE_STATUS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", cfg, errors.New("invalid MOCK_FAILURE_STATUS")
		}
		cfg.FailureStatus = n
	}
	if v := getenv("MOCK_LATENCY_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return "", cfg, errors.New("invalid MOCK_LATENCY_MS")
		}
		cfg.LatencyMS = n
	}
	if err := cfg.Validate(); err != nil {
		return "", cfg, fmt.Errorf("invalid MOCK_* configuration: %w", err)
	}
	return addr, cfg, nil
}

func run() int {
	log := observability.NewLogger(os.Stdout, "info")
	addr, cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Error(err.Error())
		return 1
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mockprovider.New(mockprovider.Options{Config: cfg, Logger: log}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second, // latency_ms can reach 60s
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("mock provider listening", "addr", addr)

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
