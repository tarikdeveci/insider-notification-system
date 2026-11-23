// Package config loads process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"strconv"
)

// Config holds the runtime settings shared by the api and worker processes.
type Config struct {
	HTTPAddr      string // HTTP_ADDR, default ":8080"
	DatabaseURL   string // DATABASE_URL, required
	RedisURL      string // REDIS_URL, required
	LogLevel      string // LOG_LEVEL, default "info"
	PGMaxConns    int    // PG_MAX_CONNS, default 10
	RedisPoolSize int    // REDIS_POOL_SIZE, default 10
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
