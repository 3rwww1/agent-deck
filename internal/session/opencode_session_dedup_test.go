package session

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
)

func stubOpenCodePanes(t *testing.T, panes map[string]int, table map[int]procowner.ProcCommand) {
	prevTable, prevPane := readOpenCodeProcessTable, readOpenCodePanePID
	readOpenCodeProcessTable = func() map[int]procowner.ProcCommand { return table }
	readOpenCodePanePID = func(inst *Instance) int { return panes[inst.ID] }
	t.Cleanup(func() { readOpenCodeProcessTable, readOpenCodePanePID = prevTable, prevPane })
}

func duplicateOpenCodeLanes() (first, second *Instance) {
	first = &Instance{ID: "first", Tool: "opencode", OpenCodeSessionID: "ses_X", OpenCodeDetectedAt: time.Now().Add(-time.Hour)}
	second = &Instance{ID: "second", Tool: "opencode", OpenCodeSessionID: "ses_X", OpenCodeDetectedAt: time.Now()}
	return first, second
}

// The later binding wins when its pane runs the session, whether the TUI owns
// its service or attaches to the shared one with --server.
func TestOpenCodeDedup_PaneRunningTheSessionKeepsIt(t *testing.T) {
	for name, argv := range map[string][]string{
		"own service": {"/usr/local/bin/opencode", "-s", "ses_X"},
		"--server":    {"opencode", "--server", "http://127.0.0.1:4096", "--session", "ses_X"},
	} {
		t.Run(name, func(t *testing.T) {
			stubOpenCodePanes(t, map[string]int{"first": 100, "second": 200}, map[int]procowner.ProcCommand{
				101: {PPID: 100, Args: []string{"opencode", "-s", "ses_OTHER"}},
				201: {PPID: 200, Args: argv},
			})
			first, second := duplicateOpenCodeLanes()
			UpdateOpenCodeSessionsWithDedup([]*Instance{first, second})
			if second.OpenCodeSessionID != "ses_X" || first.OpenCodeSessionID != "" || !first.openCodeSessionIDCleared {
				t.Fatalf("first %q, second %q; want ses_X kept by the lane whose pane runs it",
					first.OpenCodeSessionID, second.OpenCodeSessionID)
			}
		})
	}
}

func TestOpenCodeDedup_NoRunningPaneKeepsFirstBinding(t *testing.T) {
	stubOpenCodePanes(t, nil, map[int]procowner.ProcCommand{})
	first, second := duplicateOpenCodeLanes()
	UpdateOpenCodeSessionsWithDedup([]*Instance{second, first})
	if first.OpenCodeSessionID != "ses_X" || second.OpenCodeSessionID != "" {
		t.Fatalf("first %q, second %q; want ses_X kept by the first binding",
			first.OpenCodeSessionID, second.OpenCodeSessionID)
	}
}
