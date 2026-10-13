package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

var livenessSocketSeq atomic.Int64

// livenessSocket returns a private -L socket under the isolated TMUX_TMPDIR,
// killed at cleanup.
func livenessSocket(t *testing.T) string {
	t.Helper()
	skipIfNoTmuxBinary(t)
	sock := fmt.Sprintf("t5-live-%d-%d", os.Getpid(), livenessSocketSeq.Add(1))
	t.Cleanup(func() { _ = livenessTmux(sock, "kill-server") })
	return sock
}

func livenessTmux(sock string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	full := args
	if sock != "" {
		full = append([]string{"-L", sock}, args...)
	}
	// #nosec G204 -- test-owned socket under the isolated TMUX_TMPDIR.
	out, err := exec.CommandContext(ctx, "tmux", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %v: %v: %s", full, err, out)
	}
	return nil
}

func startLivenessSession(t *testing.T, sock, name string) {
	t.Helper()
	if err := livenessTmux(sock, "new-session", "-d", "-s", name, "sleep 3600"); err != nil {
		t.Fatal(err)
	}
}

func livenessRow(name, sock string, status Status) *Instance {
	inst := &Instance{ID: name, Title: name, Tool: "shell", Status: status, CreatedAt: time.Now().Add(-time.Hour)}
	inst.tmuxSession = tmux.ReconnectSessionLazy(name, name, "", "", "inactive")
	inst.tmuxSession.SocketName = sock
	return inst
}

// spawns counts tmux subprocesses started while fn runs, after one warm-up
// refresh so a shared cache reload is not charged to fn.
func spawns(sock string, fn func()) int64 {
	_ = livenessRow("warmup", sock, StatusError).UpdateStatus()
	before := tmux.SubprocessStarts()
	fn()
	return tmux.SubprocessStarts() - before
}

func refreshAll(pass *StatusUpdatePass, rows []*Instance) {
	for _, inst := range rows {
		if pass == nil {
			_ = inst.UpdateStatus()
			continue
		}
		_ = pass.UpdateStatusOnly(inst)
	}
}

func deadRows(sock string, n int, status Status) []*Instance {
	rows := make([]*Instance, n)
	for k := range rows {
		rows[k] = livenessRow(fmt.Sprintf("gone-%d", k), sock, status)
	}
	return rows
}

// A pinned pass lists the socket once and spawns nothing per stored-dead row.
func TestStatusPassLiveness_DeadRowsCostOneListing(t *testing.T) {
	sock := livenessSocket(t)
	startLivenessSession(t, sock, "keepalive")
	const n = 6
	unpinned := deadRows(sock, n, StatusError)
	base := spawns(sock, func() { refreshAll(&StatusUpdatePass{}, unpinned) })
	pinned := deadRows(sock, n, StatusError)
	got := spawns(sock, func() { refreshAll(NewListingPass(), pinned) })
	t.Logf("%d stored-error rows absent: zero pass=%d spawns, pinned pass=%d", n, base, got)
	if base < 2*n {
		t.Fatalf("zero pass spawned %d, want >= %d (has-session plus list-panes per row)", base, 2*n)
	}
	if got != 1 {
		t.Fatalf("pinned pass spawned %d, want 1 listing", got)
	}
	for k := range pinned {
		if pinned[k].Status != unpinned[k].Status || pinned[k].Substate() != unpinned[k].Substate() {
			t.Fatalf("row %d: pinned %s/%s, unpinned %s/%s", k, pinned[k].Status, pinned[k].Substate(), unpinned[k].Status, unpinned[k].Substate())
		}
	}
}

// Nil and zero-value passes never take a snapshot: they cost what main costs.
func TestStatusPassLiveness_NilAndZeroPassUnchanged(t *testing.T) {
	sock := livenessSocket(t)
	startLivenessSession(t, sock, "keepalive")
	const n = 5
	for _, tc := range []struct {
		name string
		pass *StatusUpdatePass
	}{{"nil", nil}, {"zero", &StatusUpdatePass{}}} {
		t.Run(tc.name, func(t *testing.T) {
			rows := deadRows(sock, n, StatusError)
			got := spawns(sock, func() { refreshAll(tc.pass, rows) })
			if tc.pass != nil {
				if _, ok := tc.pass.sessionNames(sock); ok || tc.pass.liveNames != nil {
					t.Fatal("struct-literal pass took a liveness snapshot")
				}
			}
			if got < 2*n || got > 2*n+2 {
				t.Fatalf("%s pass spawned %d for %d absent error rows, want %d to %d (has-session plus list-panes each, plus shared cache reloads)", tc.name, got, n, 2*n, 2*n+2)
			}
		})
	}
}

// A session on socket B is absent from A's listing: keyed on its own socket,
// it is present and refreshed, never trusted dead.
func TestStatusPassLiveness_KeyedBySocket(t *testing.T) {
	sockA, sockB := livenessSocket(t), livenessSocket(t)
	startLivenessSession(t, sockA, "only-on-a")
	startLivenessSession(t, sockB, "only-on-b")
	onA := livenessRow("only-on-b", sockA, StatusError)
	onB := livenessRow("only-on-b", sockB, StatusError)
	pass := NewListingPass()
	refreshAll(pass, []*Instance{onA, onB})
	if onA.Status != StatusError {
		t.Fatalf("row on A status=%s, want error", onA.Status)
	}
	if onB.Status == StatusError {
		t.Fatal("row on B was marked dead from another socket's listing")
	}
}

// "" (no -L) and a configured default socket are two servers: two listings.
func TestStatusPassLiveness_EmptyAndDefaultSocketAreDistinct(t *testing.T) {
	sock := livenessSocket(t)
	prev := tmux.DefaultSocketName()
	tmux.SetDefaultSocketName(sock)
	t.Cleanup(func() { tmux.SetDefaultSocketName(prev) })
	startLivenessSession(t, sock, "on-named")
	t.Cleanup(func() { _ = livenessTmux("", "kill-session", "-t", "on-empty") })
	startLivenessSession(t, "", "on-empty")
	rows := []*Instance{
		livenessRow("on-empty", "", StatusError),
		livenessRow("on-named", " "+sock+" ", StatusError),
		livenessRow("on-named", sock, StatusError),
	}
	pass := NewListingPass()
	got := spawns(sock, func() {
		for _, r := range rows {
			_, _ = pass.sessionNames(r.tmuxSession.SocketName)
		}
	})
	if got != 2 {
		t.Fatalf("listings=%d, want 2 (\"\" and %q, whitespace trimmed)", got, sock)
	}
	if _, ok := mustNames(t, pass, "")["on-empty"]; !ok {
		t.Fatal("\"\" listing misses its own session")
	}
	if _, ok := mustNames(t, pass, sock)["on-empty"]; ok {
		t.Fatal("named socket listing answered for the \"\" server")
	}
}

func mustNames(t *testing.T, pass *StatusUpdatePass, sock string) map[string]struct{} {
	t.Helper()
	names, ok := pass.sessionNames(sock)
	if !ok {
		t.Fatalf("listing %q indeterminate", sock)
	}
	return names
}

// A stored-live row never consults the listing: it gets exactly the unpinned probes.
func TestStatusPassLiveness_StoredLiveAbsentGetsOneProbe(t *testing.T) {
	sock := livenessSocket(t)
	startLivenessSession(t, sock, "keepalive")
	base := livenessRow("gone", sock, StatusRunning)
	want := spawns(sock, func() { _ = (&StatusUpdatePass{}).UpdateStatusOnly(base) })
	inst := livenessRow("gone", sock, StatusRunning)
	got := spawns(sock, func() { _ = (NewListingPass()).UpdateStatusOnly(inst) })
	if got != want {
		t.Fatalf("pinned spawns=%d, want %d: a stored-live row skips the listing and gets the unpinned probes", got, want)
	}
	if inst.Status != StatusError || inst.Status != base.Status {
		t.Fatalf("status=%s (unpinned %s), want error after the confirm probe", inst.Status, base.Status)
	}
}

// A stored-dead row whose session came back is present: normal refresh runs.
func TestStatusPassLiveness_StoredDeadReappears(t *testing.T) {
	sock := livenessSocket(t)
	startLivenessSession(t, sock, "back")
	inst := livenessRow("back", sock, StatusError)
	_ = (NewListingPass()).UpdateStatusOnly(inst)
	if inst.Status == StatusError {
		t.Fatal("reappeared session still error")
	}
}

// Sessions created after the snapshot: live and grace rows are probed
// directly, a stored-error row keeps its stored status for this pass.
func TestStatusPassLiveness_CreatedAfterSnapshot(t *testing.T) {
	sock := livenessSocket(t)
	startLivenessSession(t, sock, "keepalive")
	pass := NewListingPass()
	if _, ok := pass.sessionNames(sock); !ok {
		t.Fatal("snapshot indeterminate")
	}

	grace := livenessRow("late-grace", sock, StatusStarting)
	grace.lastStartTime = time.Now()
	running := livenessRow("late-running", sock, StatusRunning)
	dead := livenessRow("late-error", sock, StatusError)
	for _, name := range []string{"late-grace", "late-running", "late-error"} {
		startLivenessSession(t, sock, name)
	}
	refreshAll(pass, []*Instance{grace, running, dead})
	if grace.Status == StatusError {
		t.Fatal("grace row created after the snapshot read error")
	}
	if running.Status == StatusError {
		t.Fatal("running row created after the snapshot read error")
	}
	if dead.Status != StatusError {
		t.Fatalf("stored-error row status=%s, want stored error for this pass", dead.Status)
	}
}

// Skipping the pane probe changes no verdict: the hook-status error->stopped
// rule and the unknown-exit substate match the unpinned path.
func TestStatusPassLiveness_OutputParity(t *testing.T) {
	sock := livenessSocket(t)
	startLivenessSession(t, sock, "keepalive")
	build := func(hook string, probed *int) *Instance {
		inst := livenessRow("gone-claude", sock, StatusError)
		inst.Tool = "claude"
		inst.hookStatus = hook
		inst.paneDeadExitStatusForTest = func() (int, bool) { *probed++; return 0, false }
		return inst
	}
	for _, hook := range []string{"waiting", "running", ""} {
		var basePanes, pinPanes int
		base := build(hook, &basePanes)
		_ = (&StatusUpdatePass{}).UpdateStatusOnly(base)
		pinned := build(hook, &pinPanes)
		_ = (NewListingPass()).UpdateStatusOnly(pinned)
		if base.Status != pinned.Status || base.Substate() != pinned.Substate() {
			t.Fatalf("hook %q: pinned %s/%s, unpinned %s/%s", hook, pinned.Status, pinned.Substate(), base.Status, base.Substate())
		}
		if basePanes != 1 || pinPanes != 0 {
			t.Fatalf("hook %q: pane probes unpinned=%d pinned=%d, want 1 and 0", hook, basePanes, pinPanes)
		}
	}
}

// The snapshot outlives the 2 s session cache: a dead tail reached after the
// cache expires still costs zero spawns.
func TestStatusPassLiveness_DeadTailAfterCacheExpiry(t *testing.T) {
	sock := livenessSocket(t)
	startLivenessSession(t, sock, "keepalive")
	pass := NewListingPass()
	refreshAll(pass, deadRows(sock, 1, StatusError))
	time.Sleep(2500 * time.Millisecond)
	tail := deadRows(sock, 8, StatusError)
	before := tmux.SubprocessStarts()
	refreshAll(pass, tail)
	for _, inst := range tail {
		_ = inst.Substate()
	}
	if got := tmux.SubprocessStarts() - before; got != 0 {
		t.Fatalf("dead tail after cache expiry spawned %d, want 0", got)
	}
}
