package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// `service status` prints "stopped" for any silent service; only an unregistered or exited one really is.
func TestOpenCodeServiceStatus_TellsStalledAndDisabledFromStopped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
		pid      int
		wantURL  string
		wantErr  error
	}{
		{name: "not registered", wantErr: errOpenCodeServiceStopped},
		{name: "registered process exited", pid: exitedPID(t), wantErr: errOpenCodeServiceStopped},
		{name: "registered process alive but silent", pid: os.Getpid(), wantURL: "http://127.0.0.1:49374"},
		{name: "disabled", disabled: true, pid: os.Getpid(), wantErr: errOpenCodeServiceDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "opencode")
			script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n'service status') echo stopped ;;\n'service get disabled') echo %t ;;\nesac\n", tc.disabled)
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			registerOpenCodeService(t, "http://127.0.0.1:49374/", tc.pid, "secret")

			svc, err := openCodeServiceStatus(bin)
			if !errors.Is(err, tc.wantErr) || svc.URL != tc.wantURL {
				t.Fatalf("status = %q, %v; want %q, %v", svc.URL, err, tc.wantURL, tc.wantErr)
			}
		})
	}
}

// registerOpenCodeService writes the file a 2.x service leaves for its clients; pid 0 writes none.
func registerOpenCodeService(t *testing.T, url string, pid int, password string) {
	t.Helper()
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

func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}
