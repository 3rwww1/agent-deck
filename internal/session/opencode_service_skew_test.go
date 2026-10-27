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

// useInstalledOpenCodeCLIVersion undoes TestMain's pins so a test exercises the
// real resolve-and-probe path against the stub binary it wrote.
func useInstalledOpenCodeCLIVersion(t *testing.T) {
	t.Helper()
	prevProbe := probeOpenCodeCLIVersion
	probeOpenCodeCLIVersion = openCodeCLIVersion
	prevRun := runOpenCodeVersionCommand
	runOpenCodeVersionCommand = runModelProbeCommand
	t.Cleanup(func() {
		probeOpenCodeCLIVersion = prevProbe
		runOpenCodeVersionCommand = prevRun
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
	marker, err := os.ReadFile(openCodeSkewMarker(lockDir, "2.1.0", "2.1.4"))
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

// Only positive version evidence restarts anything: a matching, unknown or older
// version on either side leaves the answering service alone.
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
		{name: "installed cli older than the server", serverVersion: "2.1.4", cliVersion: "2.1.0", cliKnown: true},
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

// Two agent-deck instances on one host resolve different CLIs, 2.1.4 and 2.1.0 (a
// version-manager shim, a second install): only the newer one restarts, so the two
// cannot take turns dropping every lane.
func TestOpenCodeServiceGuard_TwoCLIVersionsRestartOnceTotal(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true, serverVersion: "2.1.0"}
	fake.answer.Store(true)
	lockDir := t.TempDir()
	newer, older := fake.guard(lockDir), fake.guard(lockDir)

	// The 2.1.4 instance restarts the 2.1.0 service once.
	pinOpenCodeCLIVersion(t, "2.1.4", true)
	if _, ok := newer.attach("opencode"); !ok {
		t.Fatal("a skewed service answers its probe; lanes keep attaching while it is replaced")
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d, want 1 for the newer CLI", got)
	}

	// The restart landed on 2.1.4, and the 2.1.0 instance now sees skew the other
	// way: toward its older CLI, which must never restart.
	fake.serverVersion = "2.1.4"
	fake.advance(openCodeServiceHealthTTL)
	pinOpenCodeCLIVersion(t, "2.1.0", true)
	if _, ok := older.attach("opencode"); !ok {
		t.Fatal("the answering service stopped being attachable")
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d after the older CLI checked, want 1: no restart toward an older version", got)
	}

	// The loop stays closed when both check again.
	fake.advance(openCodeServiceHealthTTL)
	pinOpenCodeCLIVersion(t, "2.1.4", true)
	if _, ok := newer.attach("opencode"); !ok {
		t.Fatal("the answering service stopped being attachable")
	}
	fake.advance(openCodeServiceHealthTTL)
	pinOpenCodeCLIVersion(t, "2.1.0", true)
	if _, ok := older.attach("opencode"); !ok {
		t.Fatal("the answering service stopped being attachable")
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d across both CLIs, want 1 in total", got)
	}
	if _, err := os.Stat(openCodeSkewMarker(lockDir, "2.1.0", "2.1.4")); err != nil {
		t.Fatalf("the restarted pair left no marker: %v", err)
	}
	if _, err := os.Stat(openCodeSkewMarker(lockDir, "2.1.4", "2.1.0")); err == nil {
		t.Fatal("the downgrade pair got a marker: only the restart direction is recorded")
	}
}

// A mise shim does not change when the tool behind it is upgraded, so a memo keyed
// on the shim file holds the pre-upgrade answer: the skew check probes fresh, and
// the fresh answer decides.
func TestOpenCodeServiceGuard_SkewUsesFreshCLIVersionOverMemo(t *testing.T) {
	key, err := modelProbeBinaryKey("sh")
	if err != nil {
		t.Skip("no resolvable command to stand in for the shim")
	}
	// The memoised answer the skew check must not trust: the pre-upgrade version,
	// equal to the server's, so a memo-reading check would see no skew at all.
	openCodeVersionMemo.Store(key, openCodeVersionAnswer{major: 2, ok: true, version: "2.1.0"})
	t.Cleanup(func() { openCodeVersionMemo.Delete(key) })
	prevProbe := probeOpenCodeCLIVersion
	probeOpenCodeCLIVersion = openCodeCLIVersion
	prevRun := runOpenCodeVersionCommand
	runOpenCodeVersionCommand = func(string, ...string) ([]byte, error) {
		return []byte("opencode v2.1.4\n"), nil
	}
	t.Cleanup(func() {
		probeOpenCodeCLIVersion = prevProbe
		runOpenCodeVersionCommand = prevRun
	})

	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true, serverVersion: "2.1.0"}
	fake.answer.Store(true)
	g := fake.guard(t.TempDir())

	if _, ok := g.attach("sh"); !ok {
		t.Fatal("a skewed service answers its probe; lanes keep attaching while it is replaced")
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d, want 1: the stale memo answered 2.1.0, the fresh probe answers 2.1.4", got)
	}
}

// The server's answer runs through the same normalization as the CLI's: a prefixed
// "v2.1.0" is skew against a 2.1.4 CLI, and a prefixed "v2.1.4" is not.
func TestOpenCodeServiceGuard_SkewNormalizesServerVersion(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true, serverVersion: "v2.1.0"}
	fake.answer.Store(true)
	pinOpenCodeCLIVersion(t, "2.1.4", true)
	lockDir := t.TempDir()
	g := fake.guard(lockDir)

	if _, ok := g.attach("opencode"); !ok {
		t.Fatal("a service answering a prefixed version is attachable")
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d, want 1: a prefixed server version still counts as skew", got)
	}
	if _, err := os.Stat(openCodeSkewMarker(lockDir, "2.1.0", "2.1.4")); err != nil {
		t.Fatalf("the marker names the normalized pair: %v", err)
	}

	// The prefixed equal case stays quiet.
	fake.serverVersion = "v2.1.4"
	fake.advance(openCodeServiceHealthTTL)
	if _, ok := g.attach("opencode"); !ok {
		t.Fatal("the answering service stopped being attachable")
	}
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts = %d for a prefixed equal version, want 1", got)
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
		{name: "prefixed version field, normalized at the skew check", body: `{"version":"v2.1.0","pid":123}`, want: "v2.1.0"},
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

// compareOpenCodeVersions orders per semver: the numeric core field by field, then
// the prerelease, absent above present and numeric identifiers below alphanumeric.
func TestCompareOpenCodeVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{a: "2.1.4", b: "2.1.0", want: 1},
		{a: "2.1.0", b: "2.1.4", want: -1},
		{a: "2.1.4", b: "2.1.4", want: 0},
		{a: "2.2.0", b: "2.1.9", want: 1},
		{a: "2.1.10", b: "2.1.9", want: 1},
		{a: "2.1.4", b: "2.1.4-rc1", want: 1},
		{a: "2.1.4-rc1", b: "2.1.4", want: -1},
		{a: "2.1.4-rc2", b: "2.1.4-rc1", want: 1},
		{a: "2.1.4-rc.2", b: "2.1.4-rc.10", want: -1},
		{a: "2.1.4-1", b: "2.1.4-alpha", want: -1},
		{a: "2.1.4-rc.1.1", b: "2.1.4-rc.1", want: 1},
		{a: "0.0.0-dev-42", b: "0.0.0-dev-41", want: 1},
	} {
		if got := compareOpenCodeVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("compareOpenCodeVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// normalizeOpenCodeVersion extracts the same token from either side's answer.
func TestNormalizeOpenCodeVersion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{in: "2.1.4", want: "2.1.4"},
		{in: "v2.1.4", want: "2.1.4"},
		{in: "opencode v2.1.4", want: "2.1.4"},
		{in: "0.0.0-dev-42", want: "0.0.0-dev-42"},
		{in: "dev", want: ""},
		{in: "", want: ""},
	} {
		if got := normalizeOpenCodeVersion(tc.in); got != tc.want {
			t.Fatalf("normalizeOpenCodeVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
