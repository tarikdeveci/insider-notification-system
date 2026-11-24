package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The compose stack must use its own Postgres and Redis even when the host
// shell exports DATABASE_URL or REDIS_URL for some other project; otherwise
// `make up`, `make e2e` and `make load` write to that database.
func TestComposeIgnoresHostDatabaseAndRedisURLs(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	requireCompose(t)
	cmd := exec.Command("docker", "compose", "config", "--format", "json")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(),
		"DATABASE_URL=postgres://u:p@elsewhere.example:5432/other",
		"REDIS_URL=redis://elsewhere.example:6379/0",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config failed: %v\n%s", err, stderr.String())
	}
	var cfg struct {
		Services map[string]struct {
			Environment map[string]*string `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("decode compose config: %v", err)
	}
	for _, svc := range []string{"migrate", "api", "worker"} {
		env := cfg.Services[svc].Environment
		for _, k := range []string{"DATABASE_URL", "REDIS_URL"} {
			v, ok := env[k]
			if !ok || v == nil {
				if svc == "migrate" && k == "REDIS_URL" {
					continue
				}
				t.Fatalf("%s: %s missing", svc, k)
			}
			if strings.Contains(*v, "elsewhere.example") {
				t.Fatalf("%s: %s taken from the host shell", svc, k)
			}
		}
	}
}

// Docker kills a container after stop_grace_period (10s by default). Both
// binaries drain on SIGTERM for up to 15s, so the grace period must outlast it.
func TestComposeStopGracePeriodOutlastsShutdownTimeout(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	requireCompose(t)
	cmd := exec.Command("docker", "compose", "config", "--format", "json")
	cmd.Dir = "../.."
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config failed: %v\n%s", err, stderr.String())
	}
	var cfg struct {
		Services map[string]struct {
			StopGracePeriod json.RawMessage `json:"stop_grace_period"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("decode compose config: %v", err)
	}
	for _, svc := range []string{"api", "worker"} {
		raw := cfg.Services[svc].StopGracePeriod
		if len(raw) == 0 {
			t.Fatalf("%s: stop_grace_period is not set", svc)
		}
		var d time.Duration
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if d, err = time.ParseDuration(s); err != nil {
				t.Fatalf("%s: stop_grace_period %s: %v", svc, raw, err)
			}
		} else {
			var ns int64
			if err := json.Unmarshal(raw, &ns); err != nil {
				t.Fatalf("%s: stop_grace_period %s is neither a string nor a number", svc, raw)
			}
			d = time.Duration(ns)
		}
		if d <= 15*time.Second {
			t.Fatalf("%s: stop_grace_period %v must exceed the 15s shutdown timeout", svc, d)
		}
	}
}
