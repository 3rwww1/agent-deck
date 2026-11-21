package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestCheckOpenCodeSessionConflict_RefusesIDHeldByAnotherSession(t *testing.T) {
	holder := &session.Instance{ID: "holder-id", Title: "holder", OpenCodeSessionID: "ses_X"}
	lane := &session.Instance{ID: "lane-id", Title: "lane"}
	msg, code := checkOpenCodeSessionConflict([]*session.Instance{holder, lane}, lane, " ses_X ")
	if code != ErrCodeAlreadyExists || !strings.Contains(msg, "holder-id") {
		t.Fatalf("got (%q, %q); want ALREADY_EXISTS naming holder-id", msg, code)
	}
}

func TestCheckOpenCodeSessionConflict_AllowsOwnIDAndClear(t *testing.T) {
	lane := &session.Instance{ID: "lane-id", Title: "lane", OpenCodeSessionID: "ses_X"}
	other := &session.Instance{ID: "other-id", Title: "other", OpenCodeSessionID: "ses_Y"}
	for _, value := range []string{"ses_X", "", "ses_Z"} {
		if msg, _ := checkOpenCodeSessionConflict([]*session.Instance{lane, other}, lane, value); msg != "" {
			t.Fatalf("value %q refused: %s", value, msg)
		}
	}
}

// The save-time dedup would keep the holder and quietly clear the new binding,
// so the CLI must refuse before it reports a change.
func TestSessionSetOpenCodeSessionID_RefusesHeldIDInsteadOfUndoingIt(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess CLI test skipped in short mode")
	}
	home := t.TempDir()
	projectDir := filepath.Join(home, "proj")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, title := range []string{"holder", "lane"} {
		stdout, stderr, code := runAgentDeck(t, home, "add", "-t", title, "-c", "opencode", "--no-parent", "--json", projectDir)
		if code != 0 {
			t.Fatalf("add %s failed (exit %d)\nstdout: %s\nstderr: %s", title, code, stdout, stderr)
		}
		var added struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(stdout), &added); err != nil {
			t.Fatalf("parse add %s: %v\nstdout: %s", title, err, stdout)
		}
		ids[title] = added.ID
	}
	if stdout, stderr, code := runAgentDeck(t, home, "session", "set", ids["holder"], "opencode-session-id", "ses_X"); code != 0 {
		t.Fatalf("bind holder failed (exit %d)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	stdout, stderr, code := runAgentDeck(t, home, "session", "set", ids["lane"], "opencode-session-id", "ses_X", "--json")
	if code == 0 || !strings.Contains(stdout+stderr, ErrCodeAlreadyExists) {
		t.Fatalf("binding a held id: exit %d, want %s\nstdout: %s\nstderr: %s", code, ErrCodeAlreadyExists, stdout, stderr)
	}
	for title, want := range map[string]string{"holder": "ses_X", "lane": ""} {
		stdout, stderr, code := runAgentDeck(t, home, "session", "show", ids[title], "--json")
		if code != 0 {
			t.Fatalf("show %s failed (exit %d)\nstdout: %s\nstderr: %s", title, code, stdout, stderr)
		}
		var shown struct {
			OpenCodeSessionID string `json:"opencode_session_id"`
		}
		if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
			t.Fatalf("parse show %s: %v\nstdout: %s", title, err, stdout)
		}
		if shown.OpenCodeSessionID != want {
			t.Fatalf("%s bound to %q, want %q", title, shown.OpenCodeSessionID, want)
		}
	}
}
