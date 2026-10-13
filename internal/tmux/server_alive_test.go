package tmux

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func installServerAliveFake(t *testing.T, listSessions string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in\n*list-sessions*) " + listSessions + ";;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ResetServerAliveCacheForTest()
	t.Cleanup(ResetServerAliveCacheForTest)
}

// A loaded host can take over a second just to spawn the client, while the
// server itself answers. The sweep's own calls allow 3s, so this is not dead.
func TestIsServerAlive_SlowButAliveServerIsAlive(t *testing.T) {
	installServerAliveFake(t, "sleep 1.3; echo agentdeck_slow")
	if !IsServerAlive() {
		t.Fatal("IsServerAlive() = false for a server that answered in 1.3s")
	}
}

func TestIsServerAlive_HungServerFailsFast(t *testing.T) {
	installServerAliveFake(t, "exec sleep 30")
	start := time.Now()
	alive := IsServerAlive()
	elapsed := time.Since(start)
	if alive {
		t.Fatal("IsServerAlive() = true for a hung server")
	}
	if elapsed > serverAliveProbeTimeout+time.Second {
		t.Fatalf("hung probe took %s, want about %s", elapsed, serverAliveProbeTimeout)
	}
	start = time.Now()
	if IsServerAlive() || time.Since(start) > 100*time.Millisecond {
		t.Fatal("dead verdict was not cached")
	}
}

func TestIsServerAlive_ExplicitDeadOutputIsDead(t *testing.T) {
	for _, msg := range []string{"server exited unexpectedly", "lost server"} {
		t.Run(msg, func(t *testing.T) {
			installServerAliveFake(t, "echo '"+msg+"' >&2; exit 1")
			start := time.Now()
			if IsServerAlive() {
				t.Fatalf("IsServerAlive() = true on %q", msg)
			}
			if time.Since(start) > time.Second {
				t.Fatal("explicit dead output did not fail fast")
			}
		})
	}
}
