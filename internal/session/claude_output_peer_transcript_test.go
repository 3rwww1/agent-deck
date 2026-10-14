package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// A lane that has not replied yet must not adopt a peer's transcript in its
// directory: once saved, its restart sweep would kill the peer's tmux session.
func TestClaudeDiskScan_NeverAdoptsTranscriptBoundToPeer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	projectPath := filepath.Join(home, "shared")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(projectPath); err == nil {
		projectPath = resolved
	}
	projectsDir := filepath.Join(home, ".claude", "projects", ConvertToClaudeDirName(projectPath))
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const ownID, peerID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	writeTranscript(t, projectsDir, peerID, fmt.Sprintf(
		`{"type":"assistant","cwd":%q,"sessionId":%q,"timestamp":"2026-10-14T08:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"PEER REPLY"}]}}`+"\n",
		projectPath, peerID))

	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *show-environment*CLAUDE_SESSION_ID*) echo CLAUDE_SESSION_ID=" + ownID + ";; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	lane := NewInstance("lane", projectPath)
	lane.Tool, lane.ClaudeSessionID = "claude", ownID
	lane.LastStartedAt = time.Now().Add(-time.Minute)
	lane.tmuxSession = &tmux.Session{Name: "agentdeck_lane"}
	peer := NewInstance("peer", projectPath)
	peer.Tool, peer.ClaudeSessionID, peer.Status = "claude", peerID, StatusRunning

	if resp, err := lane.GetLastResponseBestEffortChecked([]*Instance{lane, peer}); err != nil || strings.Contains(resp.Content, "PEER REPLY") {
		t.Errorf("lane output = %+v, err %v; want the lane's own (empty) output", resp, err)
	}
	lane.UpdateClaudeSession(nil)
	storage := newTestStorage(t)
	if err := storage.SaveWithGroups([]*Instance{lane}, NewGroupTreeWithGroups([]*Instance{lane}, nil)); err != nil {
		t.Fatal(err)
	}
	if loaded, _, err := storage.LoadWithGroups(); err != nil || len(loaded) != 1 || loaded[0].ClaudeSessionID != ownID {
		t.Errorf("saved claude_session_id is not the lane's own %s (loaded %v, err %v)", ownID, loaded, err)
	}
	calls, restore := withSpyKiller(t)
	defer restore()
	lane.sweepDuplicateToolSessions()
	if call := findSweepCall(*calls, "CLAUDE_SESSION_ID"); call != nil && call.envValue == peerID {
		t.Errorf("restart sweep targets the peer's session %s: it would kill the peer's tmux session", peerID)
	}
}

func TestSweepDuplicateToolSessions_SkipsDiskScannedClaudeID(t *testing.T) {
	calls, restore := withSpyKiller(t)
	defer restore()
	lane := NewInstanceWithTool("lane", "/tmp/x", "claude")
	lane.tmuxSession = &tmux.Session{Name: "agentdeck_lane"}
	lane.adoptDiscoveredClaudeSessionID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	lane.sweepDuplicateToolSessions()
	if call := findSweepCall(*calls, "CLAUDE_SESSION_ID"); call != nil {
		t.Fatalf("restart sweep kills by a disk-scanned id: %+v", *call)
	}
}
