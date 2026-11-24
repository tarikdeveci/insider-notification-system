package config_test

import (
	"testing"
	"time"

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

// A row waits for a permit and then sends, all inside one claim lease. A wait
// bound that leaves no room for the send plus a safety margin lets the lease
// expire under a live sender, so the reaper would requeue a row mid delivery.
func TestLoadRejectsRateLimitWaitThatOutlivesLease(t *testing.T) {
	m := map[string]string{"DATABASE_URL": "postgres://x", "REDIS_URL": "redis://y"}
	m["RATELIMIT_MAX_WAIT"] = "25s"
	_, err := config.Load(env(m))
	if err == nil || !contains(err.Error(), "RATELIMIT_MAX_WAIT") || !contains(err.Error(), "lease") {
		t.Fatalf("25s wait must be rejected naming RATELIMIT_MAX_WAIT and the lease, got %v", err)
	}
	max := config.LeaseDuration - config.ProviderSendTimeout - config.LeaseSafetyMargin
	m["RATELIMIT_MAX_WAIT"] = (max + time.Second).String()
	if _, err := config.Load(env(m)); err == nil {
		t.Error("a wait one second over the allowed maximum must be rejected")
	}
	m["RATELIMIT_MAX_WAIT"] = max.String()
	if _, err := config.Load(env(m)); err != nil {
		t.Errorf("the largest allowed wait must load, got %v", err)
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

func baseEnv() map[string]string {
	return map[string]string{"DATABASE_URL": "postgres://x", "REDIS_URL": "redis://y"}
}

func TestLoadWorkerDefaults(t *testing.T) {
	c, err := config.Load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.ProviderURL != "" || c.WorkerHTTPAddr != ":8081" || c.WorkerConcurrency != 10 || c.ClaimBatch != 20 {
		t.Fatalf("worker defaults wrong: %+v", c)
	}
	if c.RateLimitMaxWait != 10*time.Second || c.RetryBaseDelay != 2*time.Second || c.RetryMaxDelay != 10*time.Minute ||
		c.MaxDeliveryAge != 24*time.Hour || c.ShutdownTimeout != 15*time.Second || c.ReaperInterval != 5*time.Second {
		t.Fatalf("worker duration defaults wrong: %+v", c)
	}
}

func TestLoadWorkerOverrides(t *testing.T) {
	m := baseEnv()
	m["PROVIDER_URL"] = "http://p:9090/"
	m["WORKER_HTTP_ADDR"] = ":9999"
	m["WORKER_CONCURRENCY_PER_CHANNEL"] = "3"
	m["CLAIM_BATCH"] = "7"
	m["RATELIMIT_MAX_WAIT"] = "250ms"
	m["RETRY_BASE_DELAY"] = "1s"
	m["RETRY_MAX_DELAY"] = "2m"
	m["MAX_DELIVERY_AGE"] = "1h"
	m["SHUTDOWN_TIMEOUT"] = "3s"
	m["REAPER_INTERVAL"] = "500ms"
	c, err := config.Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.ProviderURL != "http://p:9090/" || c.WorkerHTTPAddr != ":9999" || c.WorkerConcurrency != 3 || c.ClaimBatch != 7 {
		t.Fatalf("worker overrides wrong: %+v", c)
	}
	if c.RateLimitMaxWait != 250*time.Millisecond || c.RetryBaseDelay != time.Second || c.RetryMaxDelay != 2*time.Minute ||
		c.MaxDeliveryAge != time.Hour || c.ShutdownTimeout != 3*time.Second || c.ReaperInterval != 500*time.Millisecond {
		t.Fatalf("worker duration overrides wrong: %+v", c)
	}
}

func TestLoadWorkerRejectsBadValuesNamingVariableOnly(t *testing.T) {
	durations := []string{"RATELIMIT_MAX_WAIT", "RETRY_BASE_DELAY", "RETRY_MAX_DELAY", "MAX_DELIVERY_AGE", "SHUTDOWN_TIMEOUT", "REAPER_INTERVAL"}
	for _, name := range durations {
		for _, bad := range []string{"soon-secret", "0s", "-5s"} {
			m := baseEnv()
			m[name] = bad
			_, err := config.Load(env(m))
			if err == nil || !contains(err.Error(), name) || contains(err.Error(), bad) {
				t.Errorf("%s=%q: want error naming the variable and not the value, got %v", name, bad, err)
			}
		}
	}
	for _, name := range []string{"WORKER_CONCURRENCY_PER_CHANNEL", "CLAIM_BATCH"} {
		for _, bad := range []string{"many-secret", "0", "-1"} {
			m := baseEnv()
			m[name] = bad
			_, err := config.Load(env(m))
			if err == nil || !contains(err.Error(), name) || contains(err.Error(), bad) {
				t.Errorf("%s=%q: want error naming the variable and not the value, got %v", name, bad, err)
			}
		}
	}
}

func TestLoadBreakerDefaultsAndOverrides(t *testing.T) {
	c, err := config.Load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.BreakerThreshold != 10 || c.BreakerOpenFor != 30*time.Second {
		t.Fatalf("breaker defaults wrong: %d %v", c.BreakerThreshold, c.BreakerOpenFor)
	}
	m := baseEnv()
	m["BREAKER_THRESHOLD"] = "3"
	m["BREAKER_OPEN_FOR"] = "5s"
	c, err = config.Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.BreakerThreshold != 3 || c.BreakerOpenFor != 5*time.Second {
		t.Fatalf("breaker overrides wrong: %d %v", c.BreakerThreshold, c.BreakerOpenFor)
	}
}

func TestLoadBreakerRejectsBadValuesNamingVariableOnly(t *testing.T) {
	cases := map[string][]string{
		"BREAKER_THRESHOLD": {"many-secret", "0", "-1"},
		"BREAKER_OPEN_FOR":  {"soon-secret", "0s", "-5s"},
	}
	for name, bads := range cases {
		for _, bad := range bads {
			m := baseEnv()
			m[name] = bad
			_, err := config.Load(env(m))
			if err == nil || !contains(err.Error(), name) || contains(err.Error(), bad) {
				t.Errorf("%s=%q: want error naming the variable and not the value, got %v", name, bad, err)
			}
		}
	}
}
