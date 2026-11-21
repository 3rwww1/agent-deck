package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func stubOpenCodePanes(t *testing.T, panes map[string]int, table map[int]procowner.ProcCommand) {
	prevTable, prevPane := readOpenCodeProcessTable, readOpenCodePanePID
	readOpenCodeProcessTable = func() map[int]procowner.ProcCommand { return table }
	readOpenCodePanePID = func(inst *Instance) int { return panes[inst.ID] }
	t.Cleanup(func() { readOpenCodeProcessTable, readOpenCodePanePID = prevTable, prevPane })
}

// saveAndLoadOpenCodeLanes saves lanes on their own, so a duplicate reaches
// disk the way a pre-dedup build wrote it, then loads every lane back.
func saveAndLoadOpenCodeLanes(t *testing.T, storage *Storage, lanes []*Instance) []*Instance {
	t.Helper()
	if lanes != nil {
		if err := storage.SaveWithGroups(lanes, NewGroupTreeWithGroups(lanes, nil)); err != nil {
			t.Fatalf("SaveWithGroups: %v", err)
		}
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatalf("LoadWithGroups: %v", err)
	}
	return loaded
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

// A duplicate already on disk is cleared at load, before a restart can sweep
// the sibling: the cleared lane loses the id in tmux too, and a save keeps it.
func TestOpenCodeDedup_LoadedDuplicateClearedBeforeRestartSweep(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	tmuxLog := filepath.Join(bin, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + tmuxLog + "\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stubOpenCodePanes(t, nil, map[int]procowner.ProcCommand{})
	storage := newTestStorage(t)
	for age, title := range []string{"second", "first"} {
		inst := NewInstance(title, "/tmp")
		inst.Tool, inst.OpenCodeSessionID = "opencode", "ses_X"
		inst.OpenCodeDetectedAt = time.Now().Add(-time.Duration(age) * time.Hour)
		saveAndLoadOpenCodeLanes(t, storage, []*Instance{inst})
	}

	loaded := saveAndLoadOpenCodeLanes(t, storage, nil)
	byTitle := map[string]*Instance{}
	for _, inst := range loaded {
		byTitle[inst.Title] = inst
	}
	second := byTitle["second"]
	if byTitle["first"].OpenCodeSessionID != "ses_X" || second.OpenCodeSessionID != "" {
		t.Fatalf("after load: first %q, second %q; want ses_X on first only",
			byTitle["first"].OpenCodeSessionID, second.OpenCodeSessionID)
	}
	calls, restore := withSpyKiller(t)
	defer restore()
	second.sweepDuplicateToolSessions()
	if call := findSweepCall(*calls, "OPENCODE_SESSION_ID"); call != nil {
		t.Fatalf("cleared lane still sweeps by session id: %+v", *call)
	}
	logged, _ := os.ReadFile(tmuxLog)
	if want := "set-environment -u -t " + second.GetTmuxSession().Name + " OPENCODE_SESSION_ID"; !strings.Contains(string(logged), want) {
		t.Fatalf("tmux calls do not unset the cleared lane's id (%q):\n%s", want, logged)
	}
	for _, inst := range saveAndLoadOpenCodeLanes(t, storage, loaded) {
		if inst.Title == "second" && inst.OpenCodeSessionID != "" {
			t.Fatalf("clear lost on save: second reloads with %q", inst.OpenCodeSessionID)
		}
	}
}

// Restart re-discovers before it sweeps, so a cleared 1.x lane must not take
// back the id its sibling's pane still carries: the sweep would kill the sibling.
func TestOpenCodeDedup_RestartOfClearedLaneSparesSibling(t *testing.T) {
	skipIfNoTmuxBinary(t)
	pinOpenCodeMajorVersion(t, 1, true)
	projectDir := t.TempDir()
	stubDir := t.TempDir()
	now := time.Now().UnixMilli()
	listing := fmt.Sprintf(`[{"id":"ses_X","directory":%q,"created":%d,"updated":%d}]`, projectDir, now-60_000, now)
	stub := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = session ] && [ \"$2\" = list ]; then printf '%%s\\n' %q; exit 0; fi\nsleep 30\n", listing)
	if err := os.WriteFile(filepath.Join(stubDir, "opencode"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	stubPATH := stubDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", stubPATH)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	sibling, lane := tmux.SessionPrefix+"dedup-sibling-"+suffix, tmux.SessionPrefix+"dedup-lane-"+suffix
	for _, name := range []string{sibling, lane} {
		if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", projectDir, "sleep 30").CombinedOutput(); err != nil {
			t.Fatalf("start tmux session %s: %v (%s)", name, err, strings.TrimSpace(string(out)))
		}
		t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })
	}
	if out, err := exec.Command("tmux", "set-environment", "-t", sibling, "OPENCODE_SESSION_ID", "ses_X").CombinedOutput(); err != nil {
		t.Fatalf("bind sibling pane: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	origDetect := startOpenCodeDetectionAfterRestart
	startOpenCodeDetectionAfterRestart = func(*Instance) {}
	t.Cleanup(func() { startOpenCodeDetectionAfterRestart = origDetect })

	cleared := NewInstanceWithTool("dedup-lane", projectDir, "opencode")
	cleared.ID = "dedup-lane-instance"
	cleared.tmuxSession = tmux.ReconnectSessionLazy(lane, lane, projectDir, "opencode", "waiting")
	if err := cleared.RestartWithEnv(map[string]string{"PATH": stubPATH}); err != nil {
		t.Fatalf("RestartWithEnv: %v", err)
	}

	if err := exec.Command("tmux", "has-session", "-t", sibling).Run(); err != nil {
		t.Fatalf("restarting the cleared lane killed its sibling's pane (has-session: %v)", err)
	}
	if got := cleared.OpenCodeSessionID; got == "ses_X" {
		t.Fatalf("cleared lane took back the sibling's id %q", got)
	}
}
