package config_test

import (
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"DATABASE_URL": "postgres://x", "REDIS_URL": "redis://y"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.LogLevel != "info" || c.PGMaxConns != 10 || c.RedisPoolSize != 10 {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestLoadRequiresURLs(t *testing.T) {
	if _, err := config.Load(env(map[string]string{"REDIS_URL": "redis://y"})); err == nil {
		t.Error("missing DATABASE_URL must fail")
	}
	if _, err := config.Load(env(map[string]string{"DATABASE_URL": "postgres://x"})); err == nil {
		t.Error("missing REDIS_URL must fail")
	}
}

func TestLoadRejectsBadInt(t *testing.T) {
	m := map[string]string{"DATABASE_URL": "postgres://x", "REDIS_URL": "redis://y", "PG_MAX_CONNS": "abc"}
	if _, err := config.Load(env(m)); err == nil {
		t.Error("non-numeric PG_MAX_CONNS must fail")
	}
	m["PG_MAX_CONNS"] = "0"
	if _, err := config.Load(env(m)); err == nil {
		t.Error("PG_MAX_CONNS below 1 must fail")
	}
}

func TestLoadOverridesAndErrorNamesVariable(t *testing.T) {
	m := map[string]string{
		"DATABASE_URL": "postgres://x", "REDIS_URL": "redis://y",
		"HTTP_ADDR": ":9000", "LOG_LEVEL": "debug", "PG_MAX_CONNS": "25", "REDIS_POOL_SIZE": "7",
	}
	c, err := config.Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":9000" || c.LogLevel != "debug" || c.PGMaxConns != 25 || c.RedisPoolSize != 7 {
		t.Fatalf("overrides wrong: %+v", c)
	}
	m["REDIS_POOL_SIZE"] = "-3"
	_, err = config.Load(env(m))
	if err == nil || !contains(err.Error(), "REDIS_POOL_SIZE") {
		t.Fatalf("error must name REDIS_POOL_SIZE, got %v", err)
	}
	_, err = config.Load(env(map[string]string{"REDIS_URL": "redis://y"}))
	if err == nil || !contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("error must name DATABASE_URL, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
