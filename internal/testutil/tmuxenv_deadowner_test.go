package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerPIDFromDirName(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"ad-sock-72399-1234567890", 72399},
		{"ad-tmux-42-7", 42},
		{"ad-sock-3687802701", 0},
		{"ad-tmux-3687802701", 0},
		{"ad-sock-12-34-56", 0},
		{"ad-sock-12-", 0},
		{"ad-sock--12", 0},
		{"ad-sock-0-12", 0},
		{"ad-sock-abc-12", 0},
		{"ad-sock-12-abc", 0},
		{"agent-deck-test-sock-12", 0},
	}
	for _, tc := range cases {
		for _, prefix := range ownedDirPrefixes {
			want := 0
			if strings.HasPrefix(tc.name, prefix) {
				want = tc.want
			}
			if got := ownerPIDFromDirName(tc.name, prefix); got != want {
				t.Errorf("ownerPIDFromDirName(%q, %q) = %d, want %d", tc.name, prefix, got, want)
			}
		}
	}
}

func TestReapDeadOwnerTmuxDirs_SkipsSymlink(t *testing.T) {
	base := shortTmuxTmpBase()
	if base == "" {
		t.Skip("no short tmp base")
	}
	deadPID := 0
	for pid := 4_000_000; pid > 3_000_000; pid-- {
		if !processAlive(pid) {
			deadPID = pid
			break
		}
	}
	if deadPID == 0 {
		t.Skip("no dead pid found")
	}
	link := filepath.Join(base, fmt.Sprintf("ad-tmux-%d-1", deadPID))
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })

	reapDeadOwnerTmuxDirs()

	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("reaper acted on symlink %s: %v", link, err)
	}
}

func TestReapDeadOwnerTmuxDirs_SkipsLiveOwner(t *testing.T) {
	requireTmuxBinary(t)
	dir, err := os.MkdirTemp(shortTmuxTmpBase(), ownedDirPattern("ad-sock-"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		KillTmuxServersUnder(dir)
		_ = os.RemoveAll(dir)
	})
	socket := filepath.Join(dir, "s")
	spawn := exec.Command("tmux", "-S", socket, "new-session", "-d", "sleep 600")
	spawn.Env = envWithoutTmuxVars()
	if out, err := spawn.CombinedOutput(); err != nil {
		t.Skipf("could not spawn probe tmux server: %v: %s", err, out)
	}

	reapDeadOwnerTmuxDirs()

	if tmuxPidOnSocket(t, socket) == 0 {
		t.Fatalf("reaper killed the server of live owner pid %d on %s", os.Getpid(), socket)
	}
}
