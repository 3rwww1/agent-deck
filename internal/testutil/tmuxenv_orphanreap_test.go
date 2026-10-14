package testutil

// Regression coverage for tmux servers orphaned by a test binary that died
// before its cleanup ran. A `new-session -d` server daemonizes, so it outlives
// the binary, and t.Cleanup never runs on SIGKILL, the -timeout panic, or
// default SIGINT/SIGTERM handling. The next IsolateTmuxSocket call must reap
// it. Only symbols that exist before the fix are used, so this test is red on
// the pre-fix tree.

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const orphanChildEnv = "AGENT_DECK_TEST_ORPHAN_CHILD"

// TestOrphanReapChild is the re-exec target, a no-op unless orphanChildEnv is
// set (the os/exec TestHelperProcess pattern).
func TestOrphanReapChild(t *testing.T) {
	switch os.Getenv(orphanChildEnv) {
	case "spawn":
		socket, _ := ShortTmuxSocket()
		cmd := exec.Command("tmux", "-S", socket, "new-session", "-d", "sleep 3600")
		cmd.Env = envWithoutTmuxVars()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("spawn: %v: %s", err, out)
		}
		os.Stdout.WriteString("SOCKET=" + socket + "\n")
		time.Sleep(time.Hour)
	case "isolate":
		IsolateTmuxSocket()()
	}
}

func TestIsolateTmuxSocket_ReapsServerOfDeadOwner(t *testing.T) {
	requireTmuxBinary(t)
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
	}{
		{"SIGKILL", syscall.SIGKILL},
		{"SIGTERM", syscall.SIGTERM},
		{"SIGINT", syscall.SIGINT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := spawnOrphanedServer(t, tc.sig)
			pid := tmuxPidOnSocket(t, socket)
			if pid == 0 || !tmuxServerAnswers(socket) {
				t.Fatalf("no orphaned server on %s after %s; the fixture is broken", socket, tc.name)
			}

			child := exec.Command(os.Args[0], "-test.run=^TestOrphanReapChild$")
			child.Env = append(envWithoutTmuxVars(), orphanChildEnv+"=isolate")
			if out, err := child.CombinedOutput(); err != nil {
				t.Fatalf("isolate child: %v: %s", err, out)
			}

			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) && processAlive(pid) {
				time.Sleep(50 * time.Millisecond)
			}
			if processAlive(pid) {
				t.Fatalf("tmux server pid %d on %s survived IsolateTmuxSocket after its owner died by %s", pid, socket, tc.name)
			}
		})
	}
}

// spawnOrphanedServer runs a child that starts a server via ShortTmuxSocket,
// kills the child with sig, and returns the server's socket.
func spawnOrphanedServer(t *testing.T, sig syscall.Signal) string {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestOrphanReapChild$")
	child.Env = append(envWithoutTmuxVars(), orphanChildEnv+"=spawn")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	var socket string
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if s, ok := strings.CutPrefix(scanner.Text(), "SOCKET="); ok {
			socket = s
			break
		}
	}
	_ = child.Process.Signal(sig)
	_ = child.Wait()
	if socket == "" {
		t.Fatal("spawn child printed no socket")
	}
	serverPID := tmuxPidOnSocket(t, socket)
	t.Cleanup(func() {
		if serverPID > 0 && processAlive(serverPID) {
			if proc, err := os.FindProcess(serverPID); err == nil {
				_ = proc.Kill()
			}
		}
		cmd := exec.Command("tmux", "-S", socket, "kill-server")
		cmd.Env = envWithoutTmuxVars()
		_ = cmd.Run()
		if dir := filepath.Dir(socket); strings.HasPrefix(filepath.Base(dir), "ad-sock-") {
			_ = os.RemoveAll(dir)
		}
	})
	return socket
}

func tmuxServerAnswers(socket string) bool {
	cmd := exec.Command("tmux", "-S", socket, "list-sessions")
	cmd.Env = envWithoutTmuxVars()
	return cmd.Run() == nil
}
