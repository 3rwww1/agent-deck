package tmux

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func installServerAliveFake(t *testing.T, listSessions string) {
	t.Helper()
	writeFakeTmux(t, t.TempDir(), "case \"$*\" in\n*list-sessions*) "+listSessions+";;\nesac\n")
	ResetServerAliveCacheForTest(t)
}

// Under host load the client spawn alone can exceed 1s while the server answers.
func TestIsServerAlive_SlowButAliveServerIsAlive(t *testing.T) {
	installServerAliveFake(t, "sleep 1.1; echo agentdeck_slow")
	if !IsServerAlive() {
		t.Fatal("IsServerAlive() = false for a server that answered in 1.1s")
	}
}

func shortPollTimeout(t *testing.T) {
	t.Helper()
	restore := tmuxPollTimeout
	tmuxPollTimeout = 300 * time.Millisecond
	t.Cleanup(func() { tmuxPollTimeout = restore })
}

// One timeout is not proof of death: only two in a row are.
func TestIsServerAlive_HungServerDeadAfterTwoTimeouts(t *testing.T) {
	shortPollTimeout(t)
	installServerAliveFake(t, "exec sleep 30")
	for k, want := range []bool{true, false} {
		start := time.Now()
		got := IsServerAlive()
		if elapsed := time.Since(start); elapsed < tmuxPollTimeout || elapsed > tmuxPollTimeout+2*time.Second {
			t.Fatalf("probe %d took %s, want a fresh probe of about %s", k+1, elapsed, tmuxPollTimeout)
		}
		if got != want {
			t.Fatalf("probe %d: IsServerAlive() = %v, want %v", k+1, got, want)
		}
	}
	start := time.Now()
	if IsServerAlive() || time.Since(start) > 100*time.Millisecond {
		t.Fatal("dead verdict was not cached")
	}
}

func TestIsServerAlive_SuccessResetsTimeoutCount(t *testing.T) {
	shortPollTimeout(t)
	dir := t.TempDir()
	t.Setenv("ALIVE_FIXTURE", dir)
	writeFakeTmux(t, t.TempDir(), "[ -f \"$ALIVE_FIXTURE/hang\" ] && exec sleep 30\necho agentdeck_ok\n")
	ResetServerAliveCacheForTest(t)
	hang := filepath.Join(dir, "hang")
	for k, hung := range []bool{true, false, true} {
		if hung {
			if err := os.WriteFile(hang, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			_ = os.Remove(hang)
		}
		if !IsServerAlive() {
			t.Fatalf("probe %d (hung=%v): dead before two timeouts in a row", k+1, hung)
		}
		serverAliveMu.Lock()
		serverAliveTime = time.Time{}
		serverAliveMu.Unlock()
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
			if time.Since(start) >= tmuxPollTimeout {
				t.Fatal("explicit dead output waited for the timeout")
			}
		})
	}
}

func TestIsServerAlive_OverlappingTimeoutsCountOnce(t *testing.T) {
	shortPollTimeout(t)
	installServerAliveFake(t, "exec sleep 30")
	var wg sync.WaitGroup
	results := make([]bool, 2)
	for k := range results {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			results[k] = IsServerAlive()
		}(k)
	}
	wg.Wait()
	if !results[0] || !results[1] {
		t.Fatalf("two overlapping timeouts = %v, want both alive (one hang episode)", results)
	}
	if IsServerAlive() {
		t.Fatal("a second sequential timeout did not declare the server dead")
	}
}

func TestIsServerAlive_DeadVerdictStaysCachedThroughTimeout(t *testing.T) {
	shortPollTimeout(t)
	installServerAliveFake(t, "echo 'lost server' >&2; exit 1")
	if IsServerAlive() {
		t.Fatal("lost server read alive")
	}
	writeFakeTmux(t, t.TempDir(), "exec sleep 30\n")
	serverAliveMu.Lock()
	serverAliveTime = time.Time{}
	serverAliveMu.Unlock()
	if IsServerAlive() {
		t.Fatal("timeout after a dead verdict read alive")
	}
	start := time.Now()
	if IsServerAlive() || time.Since(start) > 100*time.Millisecond {
		t.Fatal("dead verdict was re-probed instead of cached")
	}
}
