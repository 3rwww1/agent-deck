package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
)

func fakeOpenCodeForkService(t *testing.T, childID string, exitCode int) string {
	t.Helper()
	argv := filepath.Join(t.TempDir(), "argv")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nprintf '%%s\\n' %q\nexit %d\n",
		argv, fmt.Sprintf(`{"data":{"id":%q,"location":{"directory":"/p"},"time":{"created":1,"updated":2}}}`, childID), exitCode)
	setFakeOpenCodePath(t, script, false)
	return argv
}

func TestOpenCodeForkV2_ForksThroughServiceAndResumesChild(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	argv := fakeOpenCodeForkService(t, "ses_child_456", 0)

	workDir := t.TempDir()
	parent := NewInstanceWithTool("oc", workDir, "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	forked, cmd, err := parent.CreateForkedOpenCodeInstanceWithOptions("oc fork", "", &OpenCodeOptions{Model: "anthropic/claude", Agent: "build"})
	if err != nil {
		t.Fatalf("CreateForkedOpenCodeInstanceWithOptions: %v", err)
	}

	if want := "cd " + shellescape.Quote(workDir) + " && opencode -s ses_child_456"; cmd != want {
		t.Fatalf("fork command = %q, want %q (no --fork, no -m/--agent on 2.x)", cmd, want)
	}
	if forked.OpenCodeSessionID != "ses_child_456" || forked.OpenCodeDetectedAt.IsZero() {
		t.Fatalf("forked instance must be bound to the child session up front; got id=%q detected=%v",
			forked.OpenCodeSessionID, forked.OpenCodeDetectedAt)
	}
	if forked.Command != "opencode" || !forked.IsForkAwaitingStart || forked.ForkStartCommand != cmd {
		t.Fatalf("deferred launch invariant broken: command=%q awaiting=%v forkCmd=%q",
			forked.Command, forked.IsForkAwaitingStart, forked.ForkStartCommand)
	}

	gotArgv, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("read argv marker: %v", err)
	}
	if want := "api\nsession.fork\n--param\nsessionID=ses_parent_123\n-d\n{}\n"; string(gotArgv) != want {
		t.Fatalf("opencode argv =\n%s\nwant\n%s", gotArgv, want)
	}
}

func TestOpenCodeForkV2_ServiceFailureIsAnError(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	fakeOpenCodeForkService(t, "ses_child_456", 1)

	parent := NewInstanceWithTool("oc", t.TempDir(), "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	if _, _, err := parent.CreateForkedOpenCodeInstanceWithOptions("oc fork", "", nil); err == nil {
		t.Fatal("expected an error when the service fork call fails, got nil")
	}
}

func TestOpenCodeForkV2_RejectsUnsafeChildID(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	fakeOpenCodeForkService(t, "ses_child; rm -rf /", 0)

	parent := NewInstanceWithTool("oc", t.TempDir(), "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	if _, err := parent.ForkOpenCodeWithOptions("oc fork", "", nil); err == nil {
		t.Fatal("expected an error for a child id that is not shell-safe, got nil")
	}
}
