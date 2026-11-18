package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// PinGoCaches exports GOCACHE and GOMODCACHE with the values the go command
// resolves for the current (real) HOME, unless the caller already set them.
//
// Call it from TestMain BEFORE IsolateHome. Neither variable is set on a
// GitHub runner (setup-go reads `go env GOCACHE`, it does not export it), so
// both default to paths under $HOME. Once IsolateHome moves HOME to a temp
// dir, every `go build` a test runs (the shared agent-deck binary of the
// session, tuitest and releasetests packages) sees an empty build cache and
// an empty module cache: it downloads every module and recompiles the whole
// dependency graph, about a minute of CPU per build on a CI runner. Pinned,
// those builds reuse the runner's restored caches and each other's work.
//
// The caches hold only Go build output and downloaded modules, never
// agent-deck state, so pinning them does not weaken the HOME isolation.
func PinGoCaches() {
	names := []string{"GOCACHE", "GOMODCACHE"}
	out, err := exec.Command("go", append([]string{"env"}, names...)...).Output()
	if err != nil {
		return
	}
	values := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for i, name := range names {
		if i < len(values) && os.Getenv(name) == "" && strings.TrimSpace(values[i]) != "" {
			_ = os.Setenv(name, strings.TrimSpace(values[i]))
		}
	}
}

// TurnOffGoTelemetry writes the Go telemetry mode file under the current user
// config dir. Call it after HOME moves to a t.TempDir() and before a go command
// runs there: otherwise a detached telemetry child can still be writing in that
// dir when the TempDir cleanup removes it. GOTELEMETRY=off does not help: the
// go command never reads that variable.
func TurnOffGoTelemetry(t testing.TB) {
	t.Helper()
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("resolving the user config dir: %v", err)
	}
	mode := filepath.Join(dir, "go", "telemetry", "mode")
	if err := os.MkdirAll(filepath.Dir(mode), 0o700); err != nil {
		t.Fatalf("creating the go telemetry dir: %v", err)
	}
	if err := os.WriteFile(mode, []byte("off"), 0o600); err != nil {
		t.Fatalf("writing the go telemetry mode file: %v", err)
	}
}
