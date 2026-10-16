package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func saveAndReloadOpenCodeLane(t *testing.T, storage *Storage, inst *Instance) *Instance {
	t.Helper()
	if err := storage.SaveWithGroups([]*Instance{inst}, NewGroupTreeWithGroups([]*Instance{inst}, nil)); err != nil {
		t.Fatalf("SaveWithGroups: %v", err)
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadWithGroups = %d instances, err %v", len(loaded), err)
	}
	return loaded[0]
}

// The id keys are sticky in tool_data, so a clear that is only an omission
// would leave the old binding on disk for the next restart to resume.
func TestSetFieldOpenCodeSessionID_ClearPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	storage := newTestStorage(t)
	inst := NewInstance("opencode-lane", "/tmp")
	inst.Tool = "opencode"
	inst.OpenCodeSessionID = "ses_OLD"
	loaded := saveAndReloadOpenCodeLane(t, storage, inst)

	if _, _, err := SetField(loaded, FieldOpenCodeSessionID, "", nil); err != nil {
		t.Fatalf("SetField clear: %v", err)
	}
	cleared := saveAndReloadOpenCodeLane(t, storage, loaded)
	if cleared.OpenCodeSessionID != "" || !cleared.OpenCodeDetectedAt.IsZero() {
		t.Fatalf("after clear and reload: id %q, detected_at %v; want both empty",
			cleared.OpenCodeSessionID, cleared.OpenCodeDetectedAt)
	}

	if _, _, err := SetField(cleared, FieldOpenCodeSessionID, "ses_NEW", nil); err != nil {
		t.Fatalf("SetField bind: %v", err)
	}
	if rebound := saveAndReloadOpenCodeLane(t, storage, cleared); rebound.OpenCodeSessionID != "ses_NEW" {
		t.Fatalf("after rebind and reload: id %q, want ses_NEW", rebound.OpenCodeSessionID)
	}
}

// Stop syncs ids from the pane env before the kill; for OpenCode that env is a
// copy of the binding, and a stale copy must not undo a rebind on disk.
func TestSyncSessionIDsFromTmux_OpenCodeEnvNeverReplacesBinding(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *show-environment*OPENCODE_SESSION_ID*) echo OPENCODE_SESSION_ID=ses_OLD;; *show-environment*) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for bound, want := range map[string]string{"ses_NEW": "ses_NEW", "": "ses_OLD"} {
		inst := &Instance{Tool: "opencode", OpenCodeSessionID: bound, tmuxSession: &tmux.Session{Name: "agentdeck_lane"}}
		inst.SyncSessionIDsFromTmux()
		if inst.OpenCodeSessionID != want {
			t.Fatalf("bound %q: after sync id %q, want %q", bound, inst.OpenCodeSessionID, want)
		}
	}
}
