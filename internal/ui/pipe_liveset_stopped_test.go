package ui

import (
	"context"
	"slices"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// A stopped row has no tmux session, so every attach attempt on it fails and burns the connect budget.
func TestReconcileLivePipes_SkipsStoppedRows(t *testing.T) {
	stopped := session.NewInstance("stopped-row", t.TempDir())
	stopped.Status = session.StatusStopped
	running := session.NewInstance("running-row", t.TempDir())
	running.Status = session.StatusRunning
	stoppedName := stopped.GetTmuxSession().Name
	runningName := running.GetTmuxSession().Name

	var asked []string
	pm := tmux.NewPipeManager(context.Background(), nil)
	pm.SetWantPipe(func(n string) bool {
		asked = append(asked, n)
		return false
	})
	prev := tmux.GetPipeManager()
	tmux.SetPipeManager(pm)
	t.Cleanup(func() {
		tmux.SetPipeManager(prev)
		pm.Close()
	})

	h := &Home{
		liveSet:            newPipeLiveSet(livePipeLRUCapacity),
		instances:          []*session.Instance{stopped, running},
		focusedSessionName: stoppedName,
	}
	h.liveSet.touch(runningName)
	h.reconcileLivePipes()

	if slices.Contains(asked, stoppedName) {
		t.Fatalf("reconciler tried to attach a pipe to stopped row %q", stoppedName)
	}
	if !slices.Contains(asked, runningName) {
		t.Fatalf("reconciler skipped running row %q, asked %v", runningName, asked)
	}
}
