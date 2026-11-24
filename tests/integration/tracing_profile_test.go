package integration_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func composeServices(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose"}, append(args, "config", "--services")...)...)
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("docker compose config unavailable: %v", err)
	}
	return strings.Fields(string(out))
}

func TestTracingProfileStartsJaeger(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	def := composeServices(t)
	if slices.Contains(def, "jaeger") {
		t.Fatalf("default stack must not include jaeger: %v", def)
	}
	for _, want := range []string{"api", "worker", "postgres", "redis"} {
		if !slices.Contains(def, want) {
			t.Fatalf("default stack lost %q: %v", want, def)
		}
	}
	withTracing := composeServices(t, "--profile", "tracing")
	if !slices.Contains(withTracing, "jaeger") {
		t.Fatalf("tracing profile must list jaeger: %v", withTracing)
	}
}
