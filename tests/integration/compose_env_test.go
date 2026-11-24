package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
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
