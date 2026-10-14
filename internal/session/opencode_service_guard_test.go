package session

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stubOpenCodeService struct {
	mu       sync.Mutex
	now      time.Time
	stopped  bool
	answer   atomic.Bool
	probes   atomic.Int32
	starts   atomic.Int32
	restarts atomic.Int32
}

func (f *stubOpenCodeService) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *stubOpenCodeService) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

// guard is one agent-deck process; guards on one lockDir share its lock like separate processes.
func (f *stubOpenCodeService) guard(lockDir string) *openCodeServiceGuard {
	g := newOpenCodeServiceGuard()
	g.now = f.clock
	g.lockDir = func() (string, error) { return lockDir, nil }
	g.status = func(string) (openCodeService, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.stopped {
			return openCodeService{}, errOpenCodeServiceStopped
		}
		return openCodeService{URL: "http://127.0.0.1:49374", Password: "secret"}, nil
	}
	g.run = func(_, action string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch action {
		case "start":
			f.starts.Add(1)
			f.stopped = false
		case "restart":
			f.restarts.Add(1)
		}
		return nil
	}
	g.healthy = func(openCodeService) bool {
		f.probes.Add(1)
		return f.answer.Load()
	}
	return g
}

func attachConcurrently(guards []*openCodeServiceGuard, perGuard int) (attached int32) {
	var wg sync.WaitGroup
	var n atomic.Int32
	for _, g := range guards {
		for range perGuard {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, ok := g.attach("opencode"); ok {
					n.Add(1)
				}
			}()
		}
	}
	wg.Wait()
	return n.Load()
}

// A dozen lanes in two processes find the service stopped: one start, and every lane attaches.
func TestOpenCodeServiceGuard_StoppedServiceStartsOnce(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), stopped: true}
	fake.answer.Store(true)
	lockDir := t.TempDir()
	guards := []*openCodeServiceGuard{fake.guard(lockDir), fake.guard(lockDir)}

	if got := attachConcurrently(guards, 6); got != 12 {
		t.Fatalf("attached %d of 12 lanes", got)
	}
	if got := fake.starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want 1", got)
	}
}

// A dozen lanes in two processes hit a stall: no restart within the grace, then exactly one.
func TestOpenCodeServiceGuard_StallRestartsOnceAfterGrace(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0)}
	lockDir := t.TempDir()
	guards := []*openCodeServiceGuard{fake.guard(lockDir), fake.guard(lockDir)}

	for elapsed := time.Duration(0); elapsed < openCodeServiceStallGrace; elapsed += 30 * time.Second {
		if got := attachConcurrently(guards, 6); got != 0 {
			t.Fatalf("attached %d lanes to a stalled service", got)
		}
		if got := fake.restarts.Load(); got != 0 {
			t.Fatalf("restarted %d times %s into a stall, want none before %s", got, elapsed, openCodeServiceStallGrace)
		}
		fake.advance(30 * time.Second)
	}

	attachConcurrently(guards, 6)
	if got := fake.restarts.Load(); got != 1 {
		t.Fatalf("restarts after the grace period = %d, want exactly 1", got)
	}
}

func TestOpenCodeServiceGuard_StallThatClearsNeverRestarts(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0)}
	g := fake.guard(t.TempDir())

	g.attach("opencode")
	fake.advance(openCodeServiceStallGrace / 2)
	fake.answer.Store(true)
	if _, ok := g.attach("opencode"); !ok {
		t.Fatal("service answers again but attach failed")
	}
	fake.answer.Store(false)
	fake.advance(openCodeServiceStallGrace)
	g.attach("opencode")
	if got := fake.restarts.Load(); got != 0 {
		t.Fatalf("restarts = %d, want 0 (two short stalls are not one long one)", got)
	}
}

// A silent service is never cached; a healthy one is probed once per TTL, however many lanes attach.
func TestOpenCodeServiceGuard_HealthyServiceIsProbedOncePerTTL(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0)}
	g := fake.guard(t.TempDir())

	if _, ok := g.attach("opencode"); ok {
		t.Fatal("attached a lane to a silent service")
	}
	fake.advance(openCodeServiceRetryGap)
	fake.answer.Store(true)
	for range 10 {
		if _, ok := g.attach("opencode"); !ok {
			t.Fatal("service answers but attach failed")
		}
	}
	if got := fake.probes.Load(); got != 2 {
		t.Fatalf("health probes = %d, want 2: the silent one, then 1 within the TTL", got)
	}
	fake.advance(openCodeServiceHealthTTL)
	g.attach("opencode")
	if got := fake.probes.Load(); got != 3 {
		t.Fatalf("health probes = %d, want 3 after the TTL", got)
	}
}

// SSE reconnects reuse the guard's checked service instead of asking the binary each time.
func TestResolveOpenCodeService_ReconnectsReuseHealthyService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/info" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"version":"2.0.25"}`)
	}))
	t.Cleanup(srv.Close)
	calls := filepath.Join(t.TempDir(), "calls")
	useOpenCodeStub(t, fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %s\n[ \"$1 $2\" = 'service status' ] && echo %s\n", calls, srv.URL))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	useFreshOpenCodeServiceGuard(t)

	inst := &Instance{Tool: "opencode"}
	for range 3 {
		if svc, err := inst.resolveOpenCodeService(); err != nil || svc.URL != srv.URL {
			t.Fatalf("resolve = %+v, %v; want %s", svc, err, srv.URL)
		}
	}
	logged, _ := os.ReadFile(calls)
	if n := strings.Count(string(logged), "service status"); n != 1 {
		t.Fatalf("service status ran %d times for 3 reconnects, want 1:\n%s", n, logged)
	}
}

// A 2.x launch attaches with --server and reads the password in the pane, never in the command.
func TestBuildOpenCodeCommand_V2AttachesToSharedService(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	const password = "registration-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); r.URL.Path != "/api/info" || !ok || user != "opencode" || pass != password {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"version":"2.0.25"}`)
	}))
	t.Cleanup(srv.Close)

	useOpenCodeStub(t, fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = 'service status' ] && echo %s\n", srv.URL))
	registerOpenCodeService(t, srv.URL, 1, password)
	useFreshOpenCodeServiceGuard(t)

	inst := &Instance{Tool: "opencode", OpenCodeSessionID: "ses_BOUND"}
	cmd := inst.buildOpenCodeCommand("opencode")
	if !strings.Contains(cmd, " --server "+srv.URL+" -s ses_BOUND") {
		t.Fatalf("launch does not attach to the shared service: %q", cmd)
	}
	if !strings.Contains(cmd, `OPENCODE_PASSWORD="$(`) || !strings.Contains(cmd, ` service get password)" `) {
		t.Fatalf("launch does not read the service password in the pane: %q", cmd)
	}
	if strings.Contains(cmd, password) {
		t.Fatalf("launch command carries the service password: %q", cmd)
	}
}

// Lanes in one sweep share a failed verdict; a disabled service is not asked again within the TTL.
func TestOpenCodeServiceGuard_SharesFailedVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reused time.Duration
	}{
		{name: "silent", reused: openCodeServiceRetryGap},
		{name: "disabled", err: errOpenCodeServiceDisabled, reused: openCodeServiceHealthTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0)}
			g := fake.guard(t.TempDir())
			statuses := 0
			g.status = func(string) (openCodeService, error) {
				statuses++
				return openCodeService{URL: "http://127.0.0.1:49374"}, tc.err
			}
			for range 5 {
				g.attach("opencode")
			}
			fake.advance(tc.reused - time.Second)
			g.attach("opencode")
			if statuses != 1 {
				t.Fatalf("status asked %d times within %s, want 1", statuses, tc.reused)
			}
			fake.advance(time.Second)
			g.attach("opencode")
			if statuses != 2 {
				t.Fatalf("status asked %d times after %s, want 2", statuses, tc.reused)
			}
		})
	}
}

// An api call targets the healthy service, its password in the env only, and otherwise runs standalone.
func TestRunOpenCodeServiceCall_TargetsSharedServiceOrStandalone(t *testing.T) {
	const password = "registration-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); r.URL.Path != "/api/info" || !ok || pass != password {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"version":"2.0.25"}`)
	}))
	t.Cleanup(srv.Close)
	calls := filepath.Join(t.TempDir(), "calls")
	bin := filepath.Join(t.TempDir(), "opencode")
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = 'service status' ] && { echo %s; exit 0; }\necho \"$* pw=${OPENCODE_PASSWORD:-}\" >> %s\n", srv.URL, calls)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_PASSWORD", "")
	registerOpenCodeService(t, srv.URL, os.Getpid(), password)

	for _, answering := range []bool{true, false} {
		if !answering {
			srv.Close()
		}
		useFreshOpenCodeServiceGuard(t)
		if _, err := runOpenCodeServiceCall(bin, t.TempDir(), "session.remove", "ses_X", ""); err != nil {
			t.Fatalf("session.remove (service answering: %v): %v", answering, err)
		}
	}
	logged, _ := os.ReadFile(calls)
	want := "api session.remove --param sessionID=ses_X --server " + srv.URL + " pw=" + password + "\n" +
		"api session.remove --param sessionID=ses_X --standalone pw=\n"
	if string(logged) != want {
		t.Fatalf("opencode api calls =\n%s\nwant\n%s", logged, want)
	}
}

// A lane launched bare runs OpenCode's own service check, which kills a silent service, so it waits instead.
func TestBuildOpenCodeCommand_V2WaitsForSilentRegisteredService(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "frozen", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		name         string
		pid          int
		disabled     bool
		answersAfter int // `service status` calls in the pane before it prints the URL; 0 is never
		wantWait     bool
	}{
		{name: "registered, answers again", pid: os.Getpid(), answersAfter: 2, wantWait: true},
		{name: "registered, never answers", pid: os.Getpid(), wantWait: true},
		{name: "not registered", pid: 0},
		{name: "disabled", pid: os.Getpid(), disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			useOpenCodeStub(t, fmt.Sprintf(`#!/bin/sh
case "$*" in
'service status') echo x >> %[1]s/status; [ %[4]d -gt 0 ] && [ "$(wc -l < %[1]s/status)" -ge %[4]d ] && echo %[2]s || echo stopped ;;
'service get disabled') echo %[3]t ;;
'service get password') echo secret ;;
*) echo "$OPENCODE_PASSWORD $*" > %[1]s/launched ;;
esac
`, dir, srv.URL, tc.disabled, tc.answersAfter))
			registerOpenCodeService(t, srv.URL, tc.pid, "secret")
			useFreshOpenCodeServiceGuard(t)

			cmd := (&Instance{Tool: "opencode", OpenCodeSessionID: "ses_BOUND"}).buildOpenCodeCommand("opencode")
			if !tc.wantWait {
				if strings.Contains(cmd, " --server ") {
					t.Fatalf("launch attaches with no live registered service: %q", cmd)
				}
				return
			}
			if !strings.Contains(cmd, " --server "+srv.URL+" -s ses_BOUND") {
				t.Fatalf("launch does not attach to the silent registered service: %q", cmd)
			}

			// sleep returns at once and records its interval, so the whole wait runs in milliseconds.
			shims := t.TempDir()
			if err := os.WriteFile(filepath.Join(shims, "sleep"), []byte("#!/bin/sh\necho \"$1\" >> "+dir+"/slept\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(filepath.Join(dir, "status"))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			pane := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
			pane.Env = append(os.Environ(), "PATH="+shims+":"+os.Getenv("PATH")+":/usr/bin:/bin")
			if out, err := pane.CombinedOutput(); err != nil {
				t.Fatalf("launch command did not finish: %v\n%s", err, out)
			}

			slept, _ := os.ReadFile(filepath.Join(dir, "slept"))
			total := 0
			for _, field := range strings.Fields(string(slept)) {
				seconds, err := strconv.Atoi(field)
				if err != nil || seconds < 5 {
					t.Fatalf("wait polls every %q s, want 5 s or more: each poll is one opencode process", field)
				}
				total += seconds
			}
			if tc.answersAfter == 0 && time.Duration(total)*time.Second < openCodeServiceStallGrace {
				t.Fatalf("wait gave up after %d s, before agent-deck restarts a stalled service (%s)", total, openCodeServiceStallGrace)
			}
			launched, _ := os.ReadFile(filepath.Join(dir, "launched"))
			if got, want := strings.TrimSpace(string(launched)), "secret --server "+srv.URL+" -s ses_BOUND"; got != want {
				t.Fatalf("pane launched %q, want %q after the wait", got, want)
			}
		})
	}
}

// A disabled setting that cannot be read fails closed: the service the user may have turned off is never started.
func TestOpenCodeServiceGuard_UnreadableDisabledSettingNeverStarts(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\ncase \"$*\" in\n'service status') echo stopped ;;\n'service get disabled') echo 'config unreadable' >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	registerOpenCodeService(t, "", 0, "")
	g := newOpenCodeServiceGuard()
	g.lockDir = func() (string, error) { return t.TempDir(), nil }
	var actions []string
	g.run = func(_, action string) error {
		actions = append(actions, action)
		return nil
	}

	if _, ok := g.attach(bin); ok {
		t.Fatal("attached a lane with no service running")
	}
	if len(actions) > 0 {
		t.Fatalf("ran %v with the disabled setting unreadable, want nothing", actions)
	}
}

// useFreshOpenCodeServiceGuard swaps in a new process-wide guard for the test.
func useFreshOpenCodeServiceGuard(t *testing.T) *openCodeServiceGuard {
	t.Helper()
	prev := sharedOpenCodeService
	sharedOpenCodeService = newOpenCodeServiceGuard()
	sharedOpenCodeService.lockDir = func() (string, error) { return t.TempDir(), nil }
	t.Cleanup(func() { sharedOpenCodeService = prev })
	return sharedOpenCodeService
}
