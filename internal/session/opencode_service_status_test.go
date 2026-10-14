package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
)

// `service status` prints "stopped" for any silent service; only an unregistered or exited one really is.
func TestOpenCodeServiceStatus_TellsStalledAndDisabledFromStopped(t *testing.T) {
	service := fakeOpenCodeServiceProcess(t)
	for _, tc := range []struct {
		name     string
		disabled bool
		pid      int
		identify bool // read the pid's command line, as outside tests
		slow     bool // `service status` waits out OpenCode's 2 s probe of a silent service
		wantURL  string
		wantErr  error
	}{
		{name: "not registered", wantErr: errOpenCodeServiceStopped},
		{name: "registered process exited", pid: exitedPID(t), wantErr: errOpenCodeServiceStopped},
		{name: "registered process alive but silent", pid: os.Getpid(), wantURL: "http://127.0.0.1:49374"},
		{name: "silent, status waits out its probe", pid: os.Getpid(), slow: true, wantURL: "http://127.0.0.1:49374"},
		{name: "registered pid reused by another process", pid: os.Getpid(), identify: true, wantErr: errOpenCodeServiceStopped},
		{name: "registered service process silent", pid: service, identify: true, wantURL: "http://127.0.0.1:49374"},
		{name: "disabled", disabled: true, pid: os.Getpid(), wantErr: errOpenCodeServiceDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := "echo stopped"
			if tc.slow {
				status = "sleep 2.2; echo stopped"
			}
			bin := filepath.Join(t.TempDir(), "opencode")
			script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n'service status') %s ;;\n'service get disabled') echo %t ;;\nesac\n", status, tc.disabled)
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			registerOpenCodeService(t, "http://127.0.0.1:49374/", tc.pid, "secret")
			if tc.identify {
				isOpenCodeServiceProcess = procowner.IsOpenCodeSharedServicePID
			}

			svc, err := openCodeServiceStatus(bin)
			if !errors.Is(err, tc.wantErr) || svc.URL != tc.wantURL {
				t.Fatalf("status = %q, %v; want %q, %v", svc.URL, err, tc.wantURL, tc.wantErr)
			}
		})
	}
}

// registerOpenCodeService writes the file a 2.x service leaves for its clients; pid 0 writes none.
// Any live pid passes for the service process, so tests can register their own.
func registerOpenCodeService(t *testing.T, url string, pid int, password string) {
	t.Helper()
	prev := isOpenCodeServiceProcess
	isOpenCodeServiceProcess = procowner.Alive
	t.Cleanup(func() { isOpenCodeServiceProcess = prev })
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	if pid == 0 {
		return
	}
	dir := filepath.Join(stateHome, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	reg := fmt.Sprintf(`{"url":%q,"pid":%d,"password":%q}`, url, pid, password)
	if err := os.WriteFile(filepath.Join(dir, "service.json"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeOpenCodeServiceProcess runs a process whose command line is `opencode serve --service`.
func fakeOpenCodeServiceProcess(t *testing.T) int {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "serve", "--service")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	return cmd.Process.Pid
}

func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}
