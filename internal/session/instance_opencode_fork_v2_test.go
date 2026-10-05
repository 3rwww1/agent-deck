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
	return fakeOpenCodeService(t, childID, exitCode, 0)
}

// fakeOpenCodeService stubs `opencode api`: session.fork replies with childID
// and exits forkExit, every other operation exits switchExit. Each call's argv
// is appended to the returned file.
func fakeOpenCodeService(t *testing.T, childID string, forkExit, switchExit int) string {
	t.Helper()
	argv := filepath.Join(t.TempDir(), "argv")
	reply := fmt.Sprintf(`{"data":{"id":%q,"location":{"directory":"/p"},"time":{"created":1,"updated":2}}}`, childID)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %q\nif [ \"$2\" = session.fork ]; then printf '%%s\\n' %q; exit %d; fi\nexit %d\n",
		argv, reply, forkExit, switchExit)
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
	want := "api\nsession.fork\n--param\nsessionID=ses_parent_123\n-d\n{}\n" +
		"api\nsession.switchModel\n--param\nsessionID=ses_child_456\n-d\n" + `{"model":{"id":"claude","providerID":"anthropic"}}` + "\n" +
		"api\nsession.switchAgent\n--param\nsessionID=ses_child_456\n-d\n" + `{"agent":"build"}` + "\n"
	if string(gotArgv) != want {
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

func TestOpenCodeForkV2_NoOverridesForksOnly(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	argv := fakeOpenCodeForkService(t, "ses_child_456", 0)

	parent := NewInstanceWithTool("oc", t.TempDir(), "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	if _, err := parent.ForkOpenCodeWithOptions("oc fork", "", &OpenCodeOptions{}); err != nil {
		t.Fatalf("ForkOpenCodeWithOptions: %v", err)
	}
	gotArgv, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("read argv marker: %v", err)
	}
	if want := "api\nsession.fork\n--param\nsessionID=ses_parent_123\n-d\n{}\n"; string(gotArgv) != want {
		t.Fatalf("opencode argv =\n%s\nwant only the fork call\n%s", gotArgv, want)
	}
}

func TestOpenCodeForkV2_OverrideFailureIsAnError(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	fakeOpenCodeService(t, "ses_child_456", 0, 1)

	parent := NewInstanceWithTool("oc", t.TempDir(), "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	if _, _, err := parent.CreateForkedOpenCodeInstanceWithOptions("oc fork", "", &OpenCodeOptions{Agent: "build"}); err == nil {
		t.Fatal("expected an error when the child agent cannot be applied, got nil")
	}
}

func TestOpenCodeForkV2_RejectsModelWithoutProvider(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	fakeOpenCodeForkService(t, "ses_child_456", 0)

	parent := NewInstanceWithTool("oc", t.TempDir(), "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	if _, err := parent.ForkOpenCodeWithOptions("oc fork", "", &OpenCodeOptions{Model: "claude"}); err == nil {
		t.Fatal("expected an error for a model without a provider prefix, got nil")
	}
}
