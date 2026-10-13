package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/testutil"
	"github.com/asheshgoplani/agent-deck/tests/eval/harness"
)

// No hooks or bound IDs: exercise the stale Codex fallback with a counting
// tmux fake. It is the only tmux on PATH and never opens a real server.
func TestListJSON_CodexProbeCount(t *testing.T) {
	sb, env, calls := listPerfFixture(t)
	started := time.Now()
	defer func() {
		data, err := os.ReadFile(calls)
		if err != nil {
			t.Error(err)
			return
		}
		count := strings.Count(string(data), "show-environment")
		t.Logf("100 stale Codex sessions: %v; show-environment subprocesses=%d", time.Since(started), count)
		if count > 500 {
			t.Errorf("show-environment subprocesses=%d, want <=500 (linear in 100 sessions)", count)
		}
		if strings.Contains(string(data), "CODEX_SESSION_ID") {
			t.Error("status listing performed native Codex session discovery")
		}
	}()
	runListFixture(t, sb, env)
}

func TestPerf_ColdStart_List100(t *testing.T) {
	testutil.SkipIfShort(t)
	sb, env, _ := listPerfFixture(t)
	budget := testutil.ColdBudget(t, 200*time.Millisecond)
	got := testutil.TrimmedMean(func() { runListFixture(t, sb, env) })
	t.Logf("list --json 100 stale Codex sessions trimmed mean=%v budget=%v", got, budget)
	if got > budget {
		t.Fatalf("list --json 100 sessions=%v, budget=%v", got, budget)
	}
}

func listPerfFixture(t *testing.T) (*harness.Sandbox, []string, string) {
	t.Helper()
	return sessionFixture(t, 100, 0)
}

// sessionFixture seeds codex then opencode sessions behind recording tmux and opencode fakes.
func sessionFixture(t *testing.T, codex, opencode int) (*harness.Sandbox, []string, string) {
	t.Helper()
	sb := harness.NewSandbox(t)
	profileDir := filepath.Join(sb.Home, ".agent-deck", "profiles", "default")
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(filepath.Join(profileDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	rows := make([]*statedb.InstanceRow, codex+opencode)
	var names, windows, panes strings.Builder
	for n := range rows {
		tool := "codex"
		if n >= codex {
			tool = "opencode"
		}
		id := fmt.Sprintf("list-perf-%03d", n)
		name := "agentdeck_" + id
		rows[n] = &statedb.InstanceRow{
			ID: id, Title: id, ProjectPath: sb.Home, GroupPath: "my-sessions",
			Tool: tool, Command: tool, Status: "waiting", TmuxSession: name,
			CreatedAt: time.Now().Add(-time.Hour), ToolData: json.RawMessage(`{}`),
		}
		fmt.Fprintln(&names, name)
		fmt.Fprintf(&windows, "%s|1|0|%s\n", name, tool)
		fmt.Fprintf(&panes, "%s|%s|0|0|0|⠋ Working\n", name, tool)
	}
	if err := db.SaveInstances(rows); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"names": names.String(), "windows": windows.String(), "panes": panes.String()} {
		if err := os.WriteFile(filepath.Join(sb.Home, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	shim := `#!/bin/sh
printf '%s\n' "$*" >> "$HOME/tmux-calls"
while [ "$#" -gt 0 ]; do
 case "$1" in
  list-windows) cat "$HOME/windows"; exit 0;;
  list-sessions) cat "$HOME/names"; exit 0;;
  list-panes) case "$*" in *'pane_pid'*) printf '0\n';; *'-a'*) cat "$HOME/panes";; *) printf '0\n';; esac; exit 0;;
  show-environment) exit 1;;
  capture-pane) printf '⠋ Working (esc to interrupt)\n'; exit 0;;
  has-session) exit 0;;
  display-message) case "$*" in *window_activity*) printf '1\n';; *) printf '0\n';; esac; exit 0;;
 esac
 shift
done
exit 0
`
	if err := os.WriteFile(filepath.Join(sb.ShimDir, "tmux"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	opencodeShim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HOME/opencode-calls\"\nprintf '[]\\n'\n"
	if err := os.WriteFile(filepath.Join(sb.ShimDir, "opencode"), []byte(opencodeShim), 0700); err != nil {
		t.Fatal(err)
	}
	return sb, perfEnv(sb), filepath.Join(sb.Home, "tmux-calls")
}

func runListFixture(t *testing.T, sb *harness.Sandbox, env []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	args := []string{"list", "--json"}
	cmd := exec.CommandContext(ctx, sb.BinPath, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	var rows []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("list JSON: %v\n%s", err, out)
	}
	if len(rows) != 100 {
		t.Fatalf("rows=%d, want 100", len(rows))
	}
	for _, row := range rows {
		if row.Status != "running" {
			t.Fatalf("%s status=%q, want running from live pane rather than stored waiting", row.ID, row.Status)
		}
	}
}

// status must count without session discovery, and -v must reuse the counting pass.
func TestStatusJSON_NoSessionDiscovery(t *testing.T) {
	plain := runStatusFixture(t, false)
	verbose := runStatusFixture(t, true)
	t.Logf("tmux calls: --json=%d --json -v=%d (%d sessions)", plain, verbose, statusFixtureSessions)
	// -v adds one substate pane capture per row on top of the counting pass.
	if verbose > plain+statusFixtureSessions {
		t.Errorf("--json -v tmux calls=%d, want <= %d (--json %d + one per row)", verbose, plain+statusFixtureSessions, plain)
	}
}

const statusFixtureCodex, statusFixtureOpenCode = 8, 2
const statusFixtureSessions = statusFixtureCodex + statusFixtureOpenCode

// runStatusFixture runs status against a fresh fixture and returns its tmux call count.
func runStatusFixture(t *testing.T, verbose bool) int {
	t.Helper()
	args := []string{"--json"}
	if verbose {
		args = append(args, "-v")
	}
	sb, env, calls := sessionFixture(t, statusFixtureCodex, statusFixtureOpenCode)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sb.BinPath, append([]string{"status"}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status %v: %v\n%s", args, err, out)
	}
	var resp struct {
		Running  int               `json:"running"`
		Total    int               `json:"total"`
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("status %v JSON: %v\n%s", args, err, out)
	}
	if resp.Total != statusFixtureSessions || resp.Running != resp.Total {
		t.Errorf("status %v running=%d total=%d, want %d/%d from live panes", args, resp.Running, resp.Total, statusFixtureSessions, statusFixtureSessions)
	}
	if verbose && len(resp.Sessions) != resp.Total {
		t.Errorf("status %v sessions=%d, total=%d; rows and counts must cover the same set", args, len(resp.Sessions), resp.Total)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, banned := range []string{"CODEX_SESSION_ID", "set-environment", "show-environment"} {
		if n := strings.Count(log, banned); n != 0 {
			t.Errorf("status %v made %d tmux calls containing %q", args, n, banned)
		}
	}
	if oc, err := os.ReadFile(filepath.Join(sb.Home, "opencode-calls")); err == nil {
		t.Errorf("status %v ran opencode:\n%s", args, oc)
	}
	return strings.Count(log, "\n")
}

func TestCountByStatusCountsAfterEach(t *testing.T) {
	inst := &session.Instance{ID: "settled", Status: session.StatusWaiting}
	var seen session.Status
	counts := countByStatus([]*session.Instance{inst}, func(i *session.Instance) {
		i.Status = session.StatusRunning
		seen = i.Status
	})
	if counts.running != 1 || counts.waiting != 0 || seen != session.StatusRunning {
		t.Fatalf("counts=%+v seen=%v, want the status each settled counted as running", counts, seen)
	}
}
