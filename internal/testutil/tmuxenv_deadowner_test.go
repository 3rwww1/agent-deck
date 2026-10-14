package testutil

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
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
	deadPID := findDeadPID(t)
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

func TestTmuxSocketsUnder_SkipsSymlinkedSubdir(t *testing.T) {
	dir := t.TempDir()
	target, err := os.MkdirTemp(shortTmuxTmpBase(), "ad-link-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(target) })
	l, err := net.Listen("unix", filepath.Join(target, "default"))
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if err := os.Symlink(target, filepath.Join(dir, "tmux-501")); err != nil {
		t.Fatal(err)
	}
	if got := tmuxSocketsUnder(dir); len(got) != 0 {
		t.Fatalf("tmuxSocketsUnder followed a symlinked subdir: %v", got)
	}
}

func findDeadPID(t *testing.T) int {
	t.Helper()
	start := 4_000_000 - os.Getpid()%500_000
	for pid := start; pid > start-500_000; pid-- {
		if !procowner.Alive(pid) {
			return pid
		}
	}
	t.Skip("no dead pid found")
	return 0
}

func TestReapDeadOwnerTmuxDirs_KeepsDirWithListener(t *testing.T) {
	base := shortTmuxTmpBase()
	if base == "" {
		t.Skip("no short tmp base")
	}
	dir := filepath.Join(base, fmt.Sprintf("ad-sock-%d-1", findDeadPID(t)))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Skipf("cannot listen: %v", err)
	}
	t.Cleanup(func() {
		_ = l.Close()
		_ = os.RemoveAll(dir)
	})

	reapDeadOwnerTmuxDirs()

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("reaper removed %s while a server still listened on it: %v", dir, err)
	}
}
