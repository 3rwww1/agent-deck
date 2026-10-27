package session

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinOpenCodeCLIVersion pins the installed-CLI answer the skew check compares against.
func pinOpenCodeCLIVersion(t *testing.T, version string, ok bool) {
	t.Helper()
	prev := probeOpenCodeCLIVersion
	probeOpenCodeCLIVersion = func(string) (string, bool) { return version, ok }
	t.Cleanup(func() { probeOpenCodeCLIVersion = prev })
}

// useInstalledOpenCodeCLIVersion undoes TestMain's pin so a test exercises the real
// resolve-and-probe path against the stub binary it wrote.
func useInstalledOpenCodeCLIVersion(t *testing.T) {
	t.Helper()
	prev := probeOpenCodeCLIVersion
	probeOpenCodeCLIVersion = openCodeCLIVersion
	openCodeVersionMemo.Clear()
	t.Cleanup(func() {
		probeOpenCodeCLIVersion = prev
		openCodeVersionMemo.Clear()
	})
}

// The upgrade incident: the service keeps answering on the version its deleted
// binary had when it was started, and the installed CLI is newer. One restart, and
// the once-marker keeps every later check in every process from repeating it.
func TestOpenCodeServiceGuard_VersionSkewRestartsOnce(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true, serverVersion: "2.1.0"}
	fake.answer.Store(true)
	pinOpenCodeCLIVersion(t, "2.1.4", true)
	lockDir := t.TempDir()
	g := fake.guard(lockDir)

	if _, ok := g.attach("opencode"); !ok {
		t.Fatal("a skewed service answers its probe; lanes keep attaching while it is replaced")
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d, want 1: the answering server's version was the evidence", got)
	}
	if got := fake.starts.Load(); got != 0 {
		t.Fatalf("starts = %d, want 0: a running service is restarted, not started", got)
	}
	marker, err := os.ReadFile(filepath.Join(lockDir, openCodeSkewRestartName))
	if err != nil {
		t.Fatalf("the restart left no once-marker in the locks dir: %v", err)
	}
	if got, want := string(marker), "2.1.0 2.1.4\n"; got != want {
		t.Fatalf("once-marker = %q, want %q: it keys the restart on the old and new version pair", got, want)
	}

	// The stub's restart did not change the server's version, so the same pair must
	// not restart again: not in this process,
	fake.advance(openCodeServiceHealthTTL)
	if _, ok := g.attach("opencode"); !ok {
		t.Fatal("the answering service stopped being attachable")
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d for the same version pair, want 1", got)
	}
	// and not in one that shares the marker file.
	second := fake.guard(lockDir)
	fake.advance(openCodeServiceHealthTTL)
	if _, ok := second.attach("opencode"); !ok {
		t.Fatal("the answering service stopped being attachable")
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d across two agent-deck processes, want 1", got)
	}
}

// A dozen lanes in two processes see the skew: one restart, and every lane attaches.
func TestOpenCodeServiceGuard_SkewRestartsOnceAcrossProcesses(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true, serverVersion: "2.1.0"}
	fake.answer.Store(true)
	pinOpenCodeCLIVersion(t, "2.1.4", true)
	lockDir := t.TempDir()
	guards := []*openCodeServiceGuard{fake.guard(lockDir), fake.guard(lockDir)}

	if got := attachConcurrently(guards, 6); got != 12 {
		t.Fatalf("attached %d of 12 lanes", got)
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d, want 1 across two agent-deck processes", got)
	}
}

// Only positive version evidence restarts anything: a matching or unknown version on
// either side leaves the answering service alone.
func TestOpenCodeServiceGuard_SameVersionNeverRestarts(t *testing.T) {
	for _, tc := range []struct {
		name          string
		serverVersion string
		cliVersion    string
		cliKnown      bool
	}{
		{name: "same version", serverVersion: "2.1.4", cliVersion: "2.1.4", cliKnown: true},
		{name: "server reports no version", serverVersion: "", cliVersion: "2.1.4", cliKnown: true},
		{name: "installed version unknown", serverVersion: "2.1.0", cliKnown: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true, serverVersion: tc.serverVersion}
			fake.answer.Store(true)
			pinOpenCodeCLIVersion(t, tc.cliVersion, tc.cliKnown)
			g := fake.guard(t.TempDir())

			if _, ok := g.attach("opencode"); !ok {
				t.Fatal("the answering service is attachable")
			}
			if got := fake.restarts.Load(); got != 0 {
				t.Fatalf("restarts = %d, want 0 without positive skew evidence", got)
			}
		})
	}
}

// A skewed server that does not answer its probe is still a stall (#55): waited
// out, never restarted.
func TestOpenCodeServiceGuard_SkewButStalledNeverRestarts(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true, serverVersion: "2.1.0"}
	pinOpenCodeCLIVersion(t, "2.1.4", true)
	g := fake.guard(t.TempDir())

	for elapsed := time.Duration(0); elapsed < 3*openCodeServiceStallGrace; elapsed += 10 * time.Second {
		if _, ok := g.attach("opencode"); ok {
			t.Fatal("attached a lane to a silent service")
		}
		fake.advance(10 * time.Second)
	}
	if got := fake.restarts.Load(); got != 0 {
		t.Fatalf("restarted a stalled service %d times, want 0 (#55: a stall is waited out)", got)
	}
}

// A restart that fails is spent: it never repeats, and the answering service stays
// attachable while the upgrade waits for a human.
func TestOpenCodeServiceGuard_FailedSkewRestartNeverLoops(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true, serverVersion: "2.1.0"}
	fake.answer.Store(true)
	fake.failRestart.Store(true)
	pinOpenCodeCLIVersion(t, "2.1.4", true)
	g := fake.guard(t.TempDir())

	for range 3 {
		if _, ok := g.attach("opencode"); !ok {
			t.Fatal("the answering service stopped being attachable")
		}
		fake.advance(openCodeServiceHealthTTL)
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restart attempts = %d, want 1: a failed restart must not loop", got)
	}
}

// A second upgrade is a new version pair and gets its own restart.
func TestOpenCodeServiceGuard_NewSkewPairRestartsAgain(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true, serverVersion: "2.1.0"}
	fake.answer.Store(true)
	lockDir := t.TempDir()
	g := fake.guard(lockDir)

	pinOpenCodeCLIVersion(t, "2.1.4", true)
	g.attach("opencode")
	fake.advance(openCodeServiceHealthTTL)
	pinOpenCodeCLIVersion(t, "2.1.5", true)
	g.attach("opencode")
	if got := fake.restarts.Load(); got != 2 {
		t.Fatalf("restarts = %d, want 2: one per version pair", got)
	}
}

// The probe carries the server's own version, and stays healthy when the answer has none.
func TestProbeOpenCodeService_ReportsServerVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "version field", body: `{"version":"2.1.0","pid":123,"urls":[]}`, want: "2.1.0"},
		{name: "no version field", body: `{"pid":123}`},
		{name: "not json", body: `ok`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			version, err := probeOpenCodeService(openCodeService{URL: srv.URL})
			if err != nil || version != tc.want {
				t.Fatalf("probe = (%q, %v), want (%q, nil)", version, err, tc.want)
			}
		})
	}
}

// The incident end to end through the real status, registration and probe: a 2.1.0
// service keeps answering after the CLI was upgraded to 2.1.4, and `opencode service
// restart` replaces it, so a second agent-deck process attaches without restarting.
func TestOpenCodeServiceGuard_UpgradedCLIRestartsSkewedService(t *testing.T) {
	const password = "registration-secret"
	info := func(version string, w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); r.URL.Path != "/api/info" || !ok || pass != password {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"version":%q,"pid":%d}`, version, os.Getpid())
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { info("2.1.0", w, r) }))
	t.Cleanup(old.Close)
	refreshed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { info("2.1.4", w, r) }))
	t.Cleanup(refreshed.Close)

	registerOpenCodeService(t, old.URL, os.Getpid(), password)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	reg := fmt.Sprintf(`{"url":%q,"pid":%d,"password":%q}`, refreshed.URL, os.Getpid(), password)
	bin := filepath.Join(dir, "opencode")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
--version) echo 'opencode v2.1.4' ;;
service)
	case "$2" in
	restart)
		echo "$*" >> %[1]s
		cp %[2]s "$XDG_STATE_HOME/opencode/service.json"
		;;
	status) sed -n 's/.*"url":"\([^"]*\)".*/\1/p' "$XDG_STATE_HOME/opencode/service.json" ;;
	esac ;;
esac
`, calls, filepath.Join(dir, "new-registration"))
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new-registration"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	useInstalledOpenCodeCLIVersion(t)
	lockDir := t.TempDir()
	newGuard := func() *openCodeServiceGuard {
		g := newOpenCodeServiceGuard()
		g.lockDir = func() (string, error) { return lockDir, nil }
		return g
	}

	svc, ok := newGuard().attach(bin)
	if !ok || svc.URL != refreshed.URL {
		t.Fatalf("attach = (%s, %v), want the restarted service at %s", svc.URL, ok, refreshed.URL)
	}
	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("the guard never ran the CLI: %v", err)
	}
	if got := strings.Count(string(logged), "restart"); got != 1 {
		t.Fatalf("service restart ran %d times, want 1:\n%s", got, logged)
	}

	if svc, ok := newGuard().attach(bin); !ok || svc.URL != refreshed.URL {
		t.Fatalf("second process attach = (%s, %v), want the restarted service at %s", svc.URL, ok, refreshed.URL)
	}
	logged, _ = os.ReadFile(calls)
	if got := strings.Count(string(logged), "restart"); got != 1 {
		t.Fatalf("service restart ran %d times across two processes, want 1", got)
	}
}
