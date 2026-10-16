package session

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var openCodeMintedIDPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func TestNewOpenCodeSessionID_UsesOpenCodeDescendingFormat(t *testing.T) {
	at := time.UnixMilli(1_790_000_000_000)
	older, sameMillis := newOpenCodeSessionID(at), newOpenCodeSessionID(at)
	newer := newOpenCodeSessionID(at.Add(time.Second))
	for _, id := range []string{older, newer} {
		if !openCodeMintedIDPattern.MatchString(id) {
			t.Fatalf("minted id %q does not match OpenCode's ses_ format", id)
		}
		if _, err := normalizeToolSessionID(FieldOpenCodeSessionID, id); err != nil {
			t.Fatalf("minted id %q rejected by session set: %v", id, err)
		}
	}
	if newer >= older {
		t.Fatalf("newer id %q must sort before older %q, as OpenCode lists newest first", newer, older)
	}
	if sameMillis[4:16] == older[4:16] {
		t.Fatalf("two ids in one millisecond share a time prefix: %q, %q", older, sameMillis)
	}
}

// A fresh 2.x lane names its own session, so two lanes in one directory never
// end up bound to the same one.
func TestBuildOpenCodeCommand_V2FreshLaunchMintsItsOwnSession(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	isolateOpenCodeConfig(t, "opencode")
	projectPath := t.TempDir()

	first := &Instance{Tool: "opencode", ProjectPath: projectPath}
	second := &Instance{Tool: "opencode", ProjectPath: projectPath}
	for _, inst := range []*Instance{first, second} {
		cmd := inst.buildOpenCodeCommand("opencode")
		if !openCodeMintedIDPattern.MatchString(inst.OpenCodeSessionID) {
			t.Fatalf("fresh 2.x launch left the lane bound to %q, want a minted ses_ id", inst.OpenCodeSessionID)
		}
		if !strings.HasSuffix(cmd, "opencode -s "+inst.OpenCodeSessionID) {
			t.Fatalf("fresh 2.x launch = %q, want it to create the minted session with -s", cmd)
		}
	}
	if first.OpenCodeSessionID == second.OpenCodeSessionID {
		t.Fatalf("two lanes in one directory were given the same session %q", first.OpenCodeSessionID)
	}
}

// Before the first prompt the minted session does not exist yet; a restart or
// a reload must resume the same id rather than mint another.
func TestOpenCodeMintedSession_RestartBeforeFirstPromptKeepsID(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	setFakeOpenCodePath(t, "#!/bin/sh\necho 'session not found' >&2\nexit 1\n", false)
	configureFakeOpenCode(t)

	inst := NewInstance("opencode-fresh", t.TempDir())
	inst.Tool = "opencode"
	inst.buildOpenCodeCommand("opencode")
	minted := inst.OpenCodeSessionID
	if minted == "" {
		t.Fatal("fresh 2.x launch minted no id")
	}

	inst.updateOpenCodeSession(true)
	if got := inst.buildOpenCodeCommand("opencode"); !strings.HasSuffix(got, "opencode -s "+minted) {
		t.Fatalf("restart before the first prompt = %q, want -s %s", got, minted)
	}

	storage := newTestStorage(t)
	if err := storage.SaveWithGroups([]*Instance{inst}, NewGroupTreeWithGroups([]*Instance{inst}, nil)); err != nil {
		t.Fatalf("SaveWithGroups: %v", err)
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadWithGroups = %d instances, err %v", len(loaded), err)
	}
	if got := loaded[0].buildOpenCodeCommand("opencode"); !strings.HasSuffix(got, "opencode -s "+minted) {
		t.Fatalf("reloaded lane launches %q, want -s %s", got, minted)
	}
}

func TestBuildOpenCodeCommand_V1FreshLaunchMintsNothing(t *testing.T) {
	pinOpenCodeMajorVersion(t, 1, true)
	isolateOpenCodeConfig(t, "opencode")

	inst := &Instance{Tool: "opencode", ProjectPath: t.TempDir()}
	if cmd := inst.buildOpenCodeCommand("opencode"); strings.Contains(cmd, " -s ") || inst.OpenCodeSessionID != "" {
		t.Fatalf("1.x fresh launch = %q, bound %q; want detection, not a minted id", cmd, inst.OpenCodeSessionID)
	}
}
