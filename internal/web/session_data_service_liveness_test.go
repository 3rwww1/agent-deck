package web

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// Stored-dead rows absent from their socket's listing cost one listing, no per-row probe.
func TestRefreshStatusesAbsentRowsUseSocketInventory(t *testing.T) {
	const n = 20
	const sock = "web-refresh-absent"
	dir := t.TempDir()
	t.Setenv("REFRESH_FIXTURE", dir)
	rows := make([]*session.Instance, n)
	for j := range rows {
		name := fmt.Sprintf("agentdeck_web_absent_%d", j)
		status := session.StatusError
		if j%2 == 1 {
			status = session.StatusStopped
		}
		rows[j] = &session.Instance{ID: name, Title: name, Tool: "shell", Status: status, ProjectPath: dir, CreatedAt: time.Now().Add(-time.Hour)}
		rows[j].SetTmuxSessionForTest(&tmux.Session{Name: name, SocketName: sock})
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$REFRESH_FIXTURE/calls"
case "$*" in *list-sessions*) printf 'agentdeck_live\n'; exit 0;; esac
printf "can't find session\n" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmux.ResetSocketSessionCacheForTest()
	t.Cleanup(tmux.ResetSocketSessionCacheForTest)

	(&SessionDataService{}).refreshStatuses(rows)

	data, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	listings := 0
	for _, call := range strings.Split(string(data), "\n") {
		if strings.Contains(call, "-L "+sock+" ") && strings.Contains(call, "list-sessions") {
			listings++
		}
	}
	calls := string(data)
	probes, paneProbes := strings.Count(calls, "has-session"), strings.Count(calls, "pane_dead_status")
	t.Logf("web refresh over %d absent rows: listings=%d has-session=%d pane-dead=%d", n, listings, probes, paneProbes)
	if listings != 1 || probes != 0 || paneProbes != 0 {
		t.Errorf("listings=%d has-session=%d pane-dead probes=%d, want 1, 0 and 0", listings, probes, paneProbes)
	}
	for j, inst := range rows {
		want := session.StatusError
		if j%2 == 1 {
			want = session.StatusStopped
		}
		if got := inst.GetStatusThreadSafe(); got != want {
			t.Errorf("%s status=%s, want %s", inst.ID, got, want)
		}
	}
}
