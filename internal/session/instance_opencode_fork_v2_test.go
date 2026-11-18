package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/asheshgoplani/agent-deck/internal/childenv"
)

func fakeOpenCodeForkService(t *testing.T, childID string, exitCode int) string {
	t.Helper()
	return fakeOpenCodeService(t, childID, exitCode, 0)
}

// fakeOpenCodeService stubs `opencode api`: session.fork replies with childID
// and exits forkExit, session.remove exits 0, every other operation exits
// switchExit. Each call's argv is appended to the returned file. The stub is
// both on PATH and the configured [opencode].command, under a fresh HOME.
func fakeOpenCodeService(t *testing.T, childID string, forkExit, switchExit int) string {
	t.Helper()
	argv := filepath.Join(t.TempDir(), "argv")
	setFakeOpenCodePath(t, fakeOpenCodeServiceScript(argv, childID, forkExit, switchExit), false)
	configureFakeOpenCode(t)
	return argv
}

func fakeOpenCodeServiceScript(argv, childID string, forkExit, switchExit int) string {
	reply := fmt.Sprintf(`{"data":{"id":%q,"location":{"directory":"/p"},"time":{"created":1,"updated":2}}}`, childID)
	return fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %q\nif [ \"$2\" = session.fork ]; then printf '%%s\\n' %q; exit %d; fi\nif [ \"$2\" = session.remove ]; then exit 0; fi\nexit %d\n",
		argv, reply, forkExit, switchExit)
}

// The 2.x service calls run the configured binary, as discovery does: with
// the stub reachable only through [opencode].command, a bare `opencode` on
// this PATH does not exist, so the fork proves the resolved path is used.
func TestOpenCodeForkV2_UsesConfiguredBinary(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	argv := filepath.Join(t.TempDir(), "argv")
	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "opencode")
	if err := os.WriteFile(stub, []byte(fakeOpenCodeServiceScript(argv, "ses_child_456", 0, 0)), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	isolateOpenCodeConfig(t, stub)

	workDir := t.TempDir()
	parent := NewInstanceWithTool("oc", workDir, "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	_, cmd, err := parent.CreateForkedOpenCodeInstanceWithOptions("oc fork", "", nil)
	if err != nil {
		t.Fatalf("CreateForkedOpenCodeInstanceWithOptions: %v", err)
	}
	// The first tmux launch must run the configured binary too: a bare
	// `opencode` is not on this PATH (or could be a 1.x binary that does not
	// know the 2.x child session).
	if !strings.HasPrefix(cmd, "cd "+shellescape.Quote(workDir)+" && { ") || !strings.HasSuffix(cmd, stub+" -s ses_child_456; }") {
		t.Fatalf("fork command must preserve work directory and configured binary: %q", cmd)
	}
	gotArgv, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("configured binary was not run: %v", err)
	}
	if want := "api\nsession.fork\n--param\nsessionID=ses_parent_123\n-d\n{}\n--standalone\n" + "api\nsession.update\n--param\nsessionID=ses_child_456\n-d\n" + `{"title":"oc fork"}` + "\n--standalone\n"; string(gotArgv) != want {
		t.Fatalf("opencode argv =\n%s\nwant\n%s", gotArgv, want)
	}
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

	if !strings.HasPrefix(cmd, "cd "+shellescape.Quote(workDir)+" && { ") || !strings.HasSuffix(cmd, GetToolCommand("opencode")+" -s ses_child_456; }") || strings.Contains(cmd, " --fork") || strings.Contains(cmd, " -m ") || strings.Contains(cmd, " --agent ") {
		t.Fatalf("fork command must preserve child resume without 1.x flags: %q", cmd)
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
	want := "api\nsession.fork\n--param\nsessionID=ses_parent_123\n-d\n{}\n--standalone\n" +
		"api\nsession.switchModel\n--param\nsessionID=ses_child_456\n-d\n" + `{"model":{"id":"claude","providerID":"anthropic"}}` + "\n--standalone\n" +
		"api\nsession.switchAgent\n--param\nsessionID=ses_child_456\n-d\n" + `{"agent":"build"}` + "\n--standalone\n" +
		"api\nsession.update\n--param\nsessionID=ses_child_456\n-d\n" + `{"title":"oc fork"}` + "\n--standalone\n"
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

func TestOpenCodeForkV2_LaunchPrerequisiteFailure(t *testing.T) {
	for _, prerequisite := range []string{"directory", "environment source"} {
		t.Run(prerequisite, func(t *testing.T) {
			shell, err := exec.LookPath("sh")
			if err != nil {
				t.Fatal(err)
			}
			pinOpenCodeMajorVersion(t, 2, true)
			argv := fakeOpenCodeForkService(t, "ses_child_456", 0)
			g := useFreshOpenCodeServiceGuard(t)
			g.registered = func() (openCodeService, error) { return openCodeService{}, errOpenCodeServiceStopped }
			g.status = func(string) (openCodeService, error) {
				return openCodeService{}, fmt.Errorf("fixture service discovery unavailable")
			}
			bin := t.TempDir()
			binding := filepath.Join(bin, "binding")
			if err := os.WriteFile(filepath.Join(bin, "agent-deck"), []byte("#!/bin/sh\nprintf 'bound\\n' > "+shellescape.Quote(binding)+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("AGENTDECK_INSTANCE_ID", "parent_lane")
			workDir := t.TempDir()
			parent := NewInstanceWithTool("oc", workDir, "opencode")
			parent.OpenCodeSessionID = "ses_parent_123"
			parent.OpenCodeDetectedAt = time.Now()
			if prerequisite == "environment source" {
				configPath, err := GetUserConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				config, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(configPath, append(config, []byte("\n[shell]\ninit_script = 'false'\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
				ClearUserConfigCache()
			}
			child, _, err := parent.CreateForkedOpenCodeInstanceWithOptionsAndWorkDir("oc fork", "", &OpenCodeOptions{}, workDir, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if prerequisite == "directory" {
				if err := os.Remove(workDir); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(argv)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			launch := exec.CommandContext(ctx, shell, "-c", child.buildOpenCodeCommand(child.consumeForkStartCommand()))
			launch.Env = childenv.ForSelfReexec()
			out, err := launch.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() <= 0 || ctx.Err() != nil {
				t.Fatalf("want nonzero shell exit for failed prerequisite, got %v: %s", err, out)
			}
			if _, err := os.Stat(binding); !os.IsNotExist(err) {
				t.Fatal("failed prerequisite reached child binding")
			}
			after, err := os.ReadFile(argv)
			if err != nil || string(after) != string(before) {
				t.Fatal("failed prerequisite reached OpenCode launch")
			}
		})
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
	if want := "api\nsession.fork\n--param\nsessionID=ses_parent_123\n-d\n{}\n--standalone\n" + "api\nsession.update\n--param\nsessionID=ses_child_456\n-d\n" + `{"title":"oc fork"}` + "\n--standalone\n"; string(gotArgv) != want {
		t.Fatalf("opencode argv =\n%s\nwant only the fork call\n%s", gotArgv, want)
	}
}

func TestOpenCodeForkV2_OverrideFailureIsAnError(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	argv := fakeOpenCodeService(t, "ses_child_456", 0, 1)

	parent := NewInstanceWithTool("oc", t.TempDir(), "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	if _, _, err := parent.CreateForkedOpenCodeInstanceWithOptions("oc fork", "", &OpenCodeOptions{Agent: "build"}); err == nil {
		t.Fatal("expected an error when the child agent cannot be applied, got nil")
	}
	gotArgv, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("read argv marker: %v", err)
	}
	if want := "api\nsession.remove\n--param\nsessionID=ses_child_456\n--standalone\n"; !strings.HasSuffix(string(gotArgv), want) {
		t.Fatalf("a child left without its overrides must be removed; opencode argv =\n%s", gotArgv)
	}
}

func TestOpenCodeForkV2_RejectsModelWithoutProvider(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	argv := fakeOpenCodeForkService(t, "ses_child_456", 0)

	parent := NewInstanceWithTool("oc", t.TempDir(), "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	if _, err := parent.ForkOpenCodeWithOptions("oc fork", "", &OpenCodeOptions{Model: "claude"}); err == nil {
		t.Fatal("expected an error for a model without a provider prefix, got nil")
	}
	if gotArgv, err := os.ReadFile(argv); err == nil {
		t.Fatalf("an invalid model must be rejected before any service call; opencode argv =\n%s", gotArgv)
	}
}

func TestOpenCodeForkV2_ChildIDSurvivesReloadAndRestart(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	fakeOpenCodeForkService(t, "ses_child_456", 0)

	parent := NewInstanceWithTool("oc", t.TempDir(), "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	forked, _, err := parent.CreateForkedOpenCodeInstanceWithOptions("oc fork", "", &OpenCodeOptions{Model: "anthropic/claude", Agent: "build"})
	if err != nil {
		t.Fatalf("CreateForkedOpenCodeInstanceWithOptions: %v", err)
	}
	encoded, err := json.Marshal(forked)
	if err != nil {
		t.Fatalf("marshal forked instance: %v", err)
	}
	reloaded := &Instance{}
	if err := json.Unmarshal(encoded, reloaded); err != nil {
		t.Fatalf("unmarshal forked instance: %v", err)
	}

	if got, want := reloaded.buildOpenCodeCommand(reloaded.Command), "opencode -s ses_child_456"; !strings.HasSuffix(got, want) {
		t.Fatalf("restart command after reload = %q, want suffix %q (child id, no 1.x flags)", got, want)
	}
}

func TestOpenCodeForkV2_NilOptionsApplyConfigDefaults(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	argv := fakeOpenCodeForkService(t, "ses_child_456", 0)

	// fakeOpenCodeForkService wrote [opencode].command into a fresh HOME; the
	// defaults join that same table.
	configPath, err := GetUserConfigPath()
	if err != nil {
		t.Fatalf("GetUserConfigPath: %v", err)
	}
	f, err := os.OpenFile(configPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	if _, err := f.WriteString("default_model = \"anthropic/claude\"\ndefault_agent = \"build\"\n"); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close config: %v", err)
	}
	ClearUserConfigCache()

	parent := NewInstanceWithTool("oc", t.TempDir(), "opencode")
	parent.OpenCodeSessionID = "ses_parent_123"
	parent.OpenCodeDetectedAt = time.Now()

	if _, err := parent.ForkOpenCodeWithOptions("oc fork", "", nil); err != nil {
		t.Fatalf("ForkOpenCodeWithOptions: %v", err)
	}
	gotArgv, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("read argv marker: %v", err)
	}
	for _, want := range []string{
		"session.switchModel\n--param\nsessionID=ses_child_456\n-d\n" + `{"model":{"id":"claude","providerID":"anthropic"}}` + "\n",
		"session.switchAgent\n--param\nsessionID=ses_child_456\n-d\n" + `{"agent":"build"}` + "\n",
	} {
		if !strings.Contains(string(gotArgv), want) {
			t.Errorf("nil options must apply [opencode] defaults like the 1.x fork; missing %q in argv:\n%s", want, gotArgv)
		}
	}
}
