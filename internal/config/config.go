// Package config loads process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Config holds the runtime settings shared by the api and worker processes.
type Config struct {
	HTTPAddr      string // HTTP_ADDR, default ":8080"
	DatabaseURL   string // DATABASE_URL, required
	RedisURL      string // REDIS_URL, required
	LogLevel      string // LOG_LEVEL, default "info"
	PGMaxConns    int    // PG_MAX_CONNS, default 10
	RedisPoolSize int    // REDIS_POOL_SIZE, default 10

	// Worker settings. The API ignores them.
	ProviderURL       string        // PROVIDER_URL, no default; the worker validates it via provider.NewClient
	WorkerHTTPAddr    string        // WORKER_HTTP_ADDR, default ":8081"
	WorkerConcurrency int           // WORKER_CONCURRENCY_PER_CHANNEL, default 10
	ClaimBatch        int           // CLAIM_BATCH, default 20
	RateLimitMaxWait  time.Duration // RATELIMIT_MAX_WAIT, default 10s
	RetryBaseDelay    time.Duration // RETRY_BASE_DELAY, default 2s
	RetryMaxDelay     time.Duration // RETRY_MAX_DELAY, default 10m
	MaxDeliveryAge    time.Duration // MAX_DELIVERY_AGE, default 24h
	ShutdownTimeout   time.Duration // SHUTDOWN_TIMEOUT, default 15s
	ReaperInterval    time.Duration // REAPER_INTERVAL, default 5s
	BreakerThreshold  int           // BREAKER_THRESHOLD, default 10
	BreakerOpenFor    time.Duration // BREAKER_OPEN_FOR, default 30s
}

// Load reads configuration through getenv (os.Getenv in production). An empty
// value means "use the default". Errors name the offending variable but never
// echo its value, since URLs can carry credentials.
func Load(getenv func(string) string) (Config, error) {
	var c Config
	c.HTTPAddr = orDefault(getenv("HTTP_ADDR"), ":8080")
	c.LogLevel = orDefault(getenv("LOG_LEVEL"), "info")

	if c.DatabaseURL = getenv("DATABASE_URL"); c.DatabaseURL == "" {
		return Config{}, errors.New("config: DATABASE_URL is required")
	}
	if c.RedisURL = getenv("REDIS_URL"); c.RedisURL == "" {
		return Config{}, errors.New("config: REDIS_URL is required")
	}

	var err error
	if c.PGMaxConns, err = positiveInt(getenv, "PG_MAX_CONNS", 10); err != nil {
		return Config{}, err
	}
	if c.RedisPoolSize, err = positiveInt(getenv, "REDIS_POOL_SIZE", 10); err != nil {
		return Config{}, err
	}
	c.ProviderURL = getenv("PROVIDER_URL")
	c.WorkerHTTPAddr = orDefault(getenv("WORKER_HTTP_ADDR"), ":8081")
	if c.WorkerConcurrency, err = positiveInt(getenv, "WORKER_CONCURRENCY_PER_CHANNEL", 10); err != nil {
		return Config{}, err
	}
	if c.ClaimBatch, err = positiveInt(getenv, "CLAIM_BATCH", 20); err != nil {
		return Config{}, err
	}
	if c.BreakerThreshold, err = positiveInt(getenv, "BREAKER_THRESHOLD", 10); err != nil {
		return Config{}, err
	}
	durations := []struct {
		dst  *time.Duration
		name string
		def  time.Duration
	}{
		{&c.RateLimitMaxWait, "RATELIMIT_MAX_WAIT", 10 * time.Second},
		{&c.RetryBaseDelay, "RETRY_BASE_DELAY", 2 * time.Second},
		{&c.RetryMaxDelay, "RETRY_MAX_DELAY", 10 * time.Minute},
		{&c.MaxDeliveryAge, "MAX_DELIVERY_AGE", 24 * time.Hour},
		{&c.ShutdownTimeout, "SHUTDOWN_TIMEOUT", 15 * time.Second},
		{&c.ReaperInterval, "REAPER_INTERVAL", 5 * time.Second},
		{&c.BreakerOpenFor, "BREAKER_OPEN_FOR", 30 * time.Second},
	}
	for _, d := range durations {
		if *d.dst, err = positiveDuration(getenv, d.name, d.def); err != nil {
			return Config{}, err
		}
	}
	return c, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func positiveInt(getenv func(string) string, name string, def int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer", name)
	}
	if n < 1 {
		return 0, fmt.Errorf("config: %s must be at least 1", name)
	}
	return n, nil
}

func positiveDuration(getenv func(string) string, name string, def time.Duration) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration such as 500ms or 10s", name)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s must be greater than zero", name)
	}
	return d, nil
}
