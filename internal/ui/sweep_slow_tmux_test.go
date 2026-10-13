package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// A slow but alive tmux must not skip the sweep, or the screen stops updating.
func TestBackgroundStatusUpdate_SlowTmuxStillSweeps(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SWEEP_FIXTURE", dir)
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$SWEEP_FIXTURE/calls"
case "$*" in
*list-sessions*) sleep 1.3; echo agentdeck_slow_sweep;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmux.ResetServerAliveCacheForTest()
	t.Cleanup(tmux.ResetServerAliveCacheForTest)
	tmux.SeedPaneInfoCacheForTest(t, nil)
	tmux.SeedWindowCacheForTest(t, nil)

	h := newHomeForSnapshotTest()
	inst := &session.Instance{ID: "slow", Title: "slow", Tool: "shell", Status: session.StatusIdle, ProjectPath: dir, CreatedAt: time.Now().Add(-time.Hour)}
	inst.SetTmuxSessionForTest(&tmux.Session{Name: "agentdeck_slow_sweep", SocketName: ""})
	h.instances = append(h.instances, inst)

	h.backgroundStatusUpdate()

	data, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(data), "list-windows") {
		t.Fatalf("sweep skipped after the probe, no cache refresh ran\n%s", data)
	}
}
