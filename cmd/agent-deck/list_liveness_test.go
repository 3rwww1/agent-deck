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
	"github.com/asheshgoplani/agent-deck/tests/eval/harness"
)

const deadFixtureErrors, deadFixtureStopped = 20, 5

// deadListFixture modes: "indeterminate" fails list-sessions, "foreign" sets $TMUX elsewhere.
func deadListFixture(t *testing.T, mode string) (*harness.Sandbox, []string, string) {
	t.Helper()
	sb := harness.NewSandbox(t)
	profileDir := filepath.Join(sb.Home, ".agent-deck", "profiles", "default")
	hooksDir := filepath.Join(sb.Home, ".agent-deck", "hooks")
	for _, dir := range []string{profileDir, hooksDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	db, err := statedb.Open(filepath.Join(profileDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	var rows []*statedb.InstanceRow
	add := func(id, tool, status string) {
		rows = append(rows, &statedb.InstanceRow{
			ID: id, Title: id, ProjectPath: sb.Home, GroupPath: "my-sessions",
			Tool: tool, Command: tool, Status: status, TmuxSession: "agentdeck_" + id,
			CreatedAt: time.Now().Add(-time.Hour), ToolData: json.RawMessage(`{}`),
		})
	}
	for n := range deadFixtureErrors {
		id := fmt.Sprintf("dead-err-%02d", n)
		tool := "shell"
		if n%2 == 0 {
			tool = "claude"
			hook := fmt.Sprintf(`{"status":"waiting","event":"Stop","ts":%d}`, time.Now().Unix())
			if err := os.WriteFile(filepath.Join(hooksDir, id+".json"), []byte(hook), 0600); err != nil {
				t.Fatal(err)
			}
		}
		add(id, tool, "error")
	}
	for n := range deadFixtureStopped {
		add(fmt.Sprintf("dead-stop-%02d", n), "shell", "stopped")
	}
	if mode == "foreign" {
		add("live-run", "shell", "running")
	}
	if err := db.SaveInstances(rows); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	listSessions := `printf 'keepalive\n'; exit 0`
	switch mode {
	case "indeterminate":
		listSessions = `printf 'server exploded\n' >&2; exit 1`
	case "foreign":
		listSessions = `case "$all" in *'-L default'*) printf 'agentdeck_live-run\nagentdeck_dead-err-01\n';; *) printf 'keepalive\n';; esac; exit 0`
	}
	shim := `#!/bin/sh
all="$*"
printf '%s\n' "$*" >> "$HOME/tmux-calls"
while [ "$#" -gt 0 ]; do
 case "$1" in
  list-windows) printf 'keepalive|1|0|sh\n'; exit 0;;
  list-sessions) ` + listSessions + `;;
  list-panes) case "$*" in *'-a'*) printf 'keepalive|sh|0|0|0|sh\n'; exit 0;; esac; printf "can't find session\n" >&2; exit 1;;
  has-session) printf "can't find session\n" >&2; exit 1;;
  display-message) printf "can't find session\n" >&2; exit 1;;
 esac
 shift
done
exit 0
`
	if err := os.WriteFile(filepath.Join(sb.ShimDir, "tmux"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	env := perfEnv(sb)
	if mode == "foreign" {
		env = append(env, "TMUX="+filepath.Join(sb.Home, "elsewhere", "outer")+",1,0")
	}
	return sb, env, filepath.Join(sb.Home, "tmux-calls")
}

type deadListRow struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Substate string `json:"substate"`
	Source   string `json:"status_source"`
}

type deadListRun struct {
	rows  map[string]deadListRow
	stats session.ListStats
	calls string
}

func runDeadList(t *testing.T, mode string) deadListRun {
	t.Helper()
	sb, env, calls := deadListFixture(t, mode)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sb.BinPath, "list", "--json", session.ListStatsFlag)
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("list: %v\n%s\n%s", err, out, stderr.String())
	}
	var rows []deadListRow
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("list JSON: %v\n%s", err, out)
	}
	run := deadListRun{rows: map[string]deadListRow{}}
	for _, row := range rows {
		run.rows[row.ID] = row
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "agent-deck-list-stats: "); ok {
			if err := json.Unmarshal([]byte(rest), &run.stats); err != nil {
				t.Fatalf("stats line %q: %v", line, err)
			}
		}
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	run.calls = string(data)
	return run
}

func countCalls(log, sub string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

// paneDeadProbes counts per-session list-panes calls, not the all-panes warmup.
func paneDeadProbes(log string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "list-panes") && !strings.Contains(line, " -a") {
			n++
		}
	}
	return n
}

// Absent stored-dead rows cost O(sockets), not 2N, with per-session output.
func TestListJSON_DeadRowsTrustTheListing(t *testing.T) {
	got := runDeadList(t, "")
	perRow := runDeadList(t, "indeterminate")
	t.Logf("listing: tmux_calls=%d has-session=%d pane probes=%d list-sessions=%d",
		got.stats.TmuxCalls, countCalls(got.calls, "has-session"), paneDeadProbes(got.calls), countCalls(got.calls, "list-sessions"))
	t.Logf("indeterminate: tmux_calls=%d has-session=%d pane probes=%d list-sessions=%d",
		perRow.stats.TmuxCalls, countCalls(perRow.calls, "has-session"), paneDeadProbes(perRow.calls), countCalls(perRow.calls, "list-sessions"))

	if n := countCalls(got.calls, "has-session"); n != 0 {
		t.Errorf("has-session calls=%d, want 0 for stored-dead rows absent from the listing", n)
	}
	if n := paneDeadProbes(got.calls); n != 0 {
		t.Errorf("per-session list-panes calls=%d, want 0", n)
	}
	if n := countCalls(got.calls, "list-sessions"); n > 2 {
		t.Errorf("list-sessions calls=%d, want <= 2 (main already spends 2: candidates plus cache warmup)", n)
	}
	if got.stats.TmuxCalls > 8 {
		t.Errorf("tmux_calls=%d, want O(sockets) <= 8", got.stats.TmuxCalls)
	}

	// An indeterminate listing keeps every per-session probe.
	if n := countCalls(perRow.calls, "has-session"); n != 2*deadFixtureErrors {
		t.Errorf("indeterminate has-session calls=%d, want %d as on main", n, 2*deadFixtureErrors)
	}
	if n := paneDeadProbes(perRow.calls); n != deadFixtureErrors {
		t.Errorf("indeterminate per-session list-panes calls=%d, want %d as on main", n, deadFixtureErrors)
	}
	if perRow.stats.TmuxCalls < 2*deadFixtureErrors {
		t.Errorf("indeterminate tmux_calls=%d, want >= %d", perRow.stats.TmuxCalls, 2*deadFixtureErrors)
	}

	stopped := 0
	for id, want := range perRow.rows {
		row := got.rows[id]
		if strings.HasPrefix(id, "dead-err-") && want.Status == "stopped" {
			stopped++
		}
		if !strings.HasPrefix(id, "dead-err-") {
			continue
		}
		if row.Status != want.Status || row.Substate != want.Substate {
			t.Errorf("%s: listing %s/%s, per-session %s/%s", id, row.Status, row.Substate, want.Status, want.Substate)
		}
	}
	if stopped == 0 {
		t.Error("no hook-status error->stopped row: the parity case is not exercised")
	}
}

// Inside another tmux server, unlisted socket-less rows keep their stored status.
func TestListJSON_ForeignServerKeepsStatus(t *testing.T) {
	got := runDeadList(t, "foreign")
	for id, want := range map[string]string{"live-run": "running", "dead-err-01": "error"} {
		if row := got.rows[id]; row.Status != want {
			t.Errorf("%s status=%q, want %q kept inside a foreign server", id, row.Status, want)
		}
	}
}
