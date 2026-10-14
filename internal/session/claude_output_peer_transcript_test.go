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

// A lane with no reply yet never reads or adopts the transcript a peer is bound to.
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
	peer.Tool, peer.ClaudeSessionID, peer.Status = "claude", strings.ToUpper(peerID), StatusRunning

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
}

// An adoption that slips past the scan's peer check never evicts the session
// that recorded the id, however old the adopting lane is.
func TestUpdateClaudeSessionsWithDedup_RecordedClaimBeatsDiskScannedClaim(t *testing.T) {
	const id = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	lane := NewInstanceWithTool("lane", "/tmp/x", "claude")
	lane.CreatedAt = time.Now().Add(-time.Hour)
	lane.adoptDiscoveredClaudeSessionID(id)
	owner := NewInstanceWithTool("owner", "/tmp/x", "claude")
	owner.ClaudeSessionID = id

	UpdateClaudeSessionsWithDedup([]*Instance{lane, owner})
	if owner.ClaudeSessionID != id || lane.ClaudeSessionID != "" {
		t.Fatalf("owner %q, lane %q; want the recorded owner to keep %s", owner.ClaudeSessionID, lane.ClaudeSessionID, id)
	}
}
