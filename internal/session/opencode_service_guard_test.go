package session

import (
	"context"
	"errors"
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
	mu         sync.Mutex
	now        time.Time
	stopped    bool
	registered bool // the registration file names a live process
	statuses   atomic.Int32
	answer     atomic.Bool
	rejects    atomic.Bool // answers the probe with an error instead of staying silent
	probes     atomic.Int32
	starts     atomic.Int32
	restarts   atomic.Int32
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
		f.statuses.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.stopped {
			return openCodeService{}, errOpenCodeServiceStopped
		}
		return openCodeService{URL: "http://127.0.0.1:49374", Password: "secret"}, nil
	}
	g.registered = func() (openCodeService, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.registered || f.stopped {
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
	g.probe = func(openCodeService) error {
		f.probes.Add(1)
		switch {
		case f.answer.Load():
			return nil
		case f.rejects.Load():
			return errors.New("opencode service health probe: HTTP 401 Unauthorized")
		}
		return errOpenCodeServiceSilent
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

// A dozen lanes in two processes hit a long stall: none attaches, and nothing restarts the service under them.
func TestOpenCodeServiceGuard_StallNeverRestarts(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0)}
	lockDir := t.TempDir()
	guards := []*openCodeServiceGuard{fake.guard(lockDir), fake.guard(lockDir)}

	for elapsed := time.Duration(0); elapsed < 10*openCodeServiceStallGrace; elapsed += 10 * time.Second {
		if got := attachConcurrently(guards, 6); got != 0 {
			t.Fatalf("attached %d lanes to a stalled service", got)
		}
		fake.advance(10 * time.Second)
	}
	if got := fake.restarts.Load(); got != 0 {
		t.Fatalf("restarted a stalled service %d times, want 0: each restart drops every lane", got)
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

// A thrashing service can still answer its health route while every session request times out.
func TestOpenCodeServiceGuard_SilentRequestsNeverRestart(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); hung.Close() })
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0)}
	fake.answer.Store(true)
	g := useFreshOpenCodeServiceGuard(t)
	stub := fake.guard(t.TempDir())
	g.now, g.run, g.probe = stub.now, stub.run, stub.probe
	g.status = func(string) (openCodeService, error) { return openCodeService{URL: hung.URL}, nil }

	for elapsed := time.Duration(0); elapsed <= openCodeServiceStallGrace+time.Minute; elapsed += 20 * time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, _, _ = callOpenCodeService(ctx, "opencode", http.MethodGet, "/api/session/ses_X", "")
		cancel()
		fake.advance(20 * time.Second)
	}
	if got := fake.restarts.Load(); got != 0 {
		t.Fatalf("restarts = %d, want 0 for a service whose requests time out for %s", got, openCodeServiceStallGrace+time.Minute)
	}
}

// A service that rejects the probe (a 401 from a password agent-deck cannot read, a 503 while it boots) is up, not stalled.
func TestOpenCodeServiceGuard_RejectedProbeNeverRestarts(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0)}
	fake.rejects.Store(true)
	g := fake.guard(t.TempDir())

	for elapsed := time.Duration(0); elapsed <= 2*openCodeServiceStallGrace; elapsed += 30 * time.Second {
		if _, ok := g.attach("opencode"); ok {
			t.Fatal("attached a lane to a service that rejects its probe")
		}
		fake.advance(30 * time.Second)
	}
	if got := fake.restarts.Load(); got != 0 {
		t.Fatalf("restarted a service that answers %d times, want 0", got)
	}
}

// Only a probe that gets no answer in time is silence; an error answer or a refused connection is not.
func TestProbeOpenCodeService_TellsSilenceFromRejection(t *testing.T) {
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); silent.Close() })
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(rejecting.Close)
	refused := httptest.NewServer(http.NotFoundHandler())
	refused.Close()

	if err := probeOpenCodeService(openCodeService{URL: silent.URL}); !errors.Is(err, errOpenCodeServiceSilent) {
		t.Fatalf("silent service: probe = %v, want %v", err, errOpenCodeServiceSilent)
	}
	for name, url := range map[string]string{"401": rejecting.URL, "refused": refused.URL} {
		if err := probeOpenCodeService(openCodeService{URL: url}); err == nil || errors.Is(err, errOpenCodeServiceSilent) {
			t.Fatalf("%s: probe = %v, want an error that is not silence", name, err)
		}
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

// A registered service is probed from its file: no `opencode service status` runtime boots for it.
func TestOpenCodeServiceGuard_RegisteredServiceSkipsTheCLI(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true}
	fake.answer.Store(true)
	g := fake.guard(t.TempDir())

	svc, ok := g.attach("opencode")
	if !ok || svc.URL != "http://127.0.0.1:49374" || svc.Password != "secret" {
		t.Fatalf("attach = %+v, %v, want the registered service", svc, ok)
	}
	if got := fake.statuses.Load(); got != 0 {
		t.Fatalf("service status asked %d times for a live registered service, want 0", got)
	}
	if got := fake.probes.Load(); got != 1 {
		t.Fatalf("health probes = %d, want 1", got)
	}
}

// A stall is re-probed at 15 s, 30 s, 60 s, then every 2 min, with no CLI until the registered process is gone.
func TestOpenCodeServiceGuard_StallBacksOffExponentially(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true}
	g := fake.guard(t.TempDir())

	for range 5 {
		if _, ok := g.attach("opencode"); ok {
			t.Fatal("attached a lane to a silent service")
		}
	}
	if got := fake.probes.Load(); got != 1 {
		t.Fatalf("health probes = %d for five attaches in one sweep, want 1", got)
	}
	if !g.stalled() {
		t.Fatal("a silent registered service does not count as stalled")
	}
	for i, wait := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute} {
		fake.advance(wait - time.Second)
		g.attach("opencode")
		if got := fake.probes.Load(); got != int32(i+1) {
			t.Fatalf("health probes = %d within wait %d (%s), want %d", got, i+1, wait, i+1)
		}
		fake.advance(time.Second)
		g.attach("opencode")
		if got := fake.probes.Load(); got != int32(i+2) {
			t.Fatalf("health probes = %d after wait %d (%s), want %d", got, i+1, wait, i+2)
		}
	}
	if got := fake.statuses.Load(); got != 0 {
		t.Fatalf("service status asked %d times while the registered service stalled, want 0", got)
	}

	// The process dies: the registration no longer names it, and the CLI finds the service stopped and starts it.
	fake.mu.Lock()
	fake.stopped = true
	fake.mu.Unlock()
	fake.answer.Store(true)
	fake.advance(openCodeServiceStallBackoffCap)
	if _, ok := g.attach("opencode"); !ok {
		t.Fatal("service restarted and answers but attach failed")
	}
	if got := fake.starts.Load(); got != 1 {
		t.Fatalf("starts = %d, want 1 once the registered process was gone", got)
	}
	if got := fake.statuses.Load(); got == 0 {
		t.Fatal("service status never asked after the registered process was gone")
	}
}

// The first healthy probe ends the backoff: the next stall starts again at 15 s.
func TestOpenCodeServiceGuard_HealthyProbeResetsTheBackoff(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true}
	g := fake.guard(t.TempDir())

	for _, wait := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute} {
		g.attach("opencode")
		fake.advance(wait)
	}
	fake.answer.Store(true)
	if _, ok := g.attach("opencode"); !ok {
		t.Fatal("service answers again but attach failed")
	}
	if g.stalled() {
		t.Fatal("still stalled after a healthy probe")
	}

	fake.answer.Store(false)
	fake.advance(openCodeServiceHealthTTL)
	g.attach("opencode")
	probes := fake.probes.Load()
	fake.advance(openCodeServiceStallBackoffBase - time.Second)
	g.attach("opencode")
	if got := fake.probes.Load(); got != probes {
		t.Fatalf("health probes = %d within the first wait of a new stall, want %d", got, probes)
	}
	fake.advance(time.Second)
	g.attach("opencode")
	if got := fake.probes.Load(); got != probes+1 {
		t.Fatalf("health probes = %d after %s, want %d: the backoff did not reset", got, openCodeServiceStallBackoffBase, probes+1)
	}
}

// A probe that hangs never blocks the guard's readers: the lock covers recorded state, not the network.
func TestOpenCodeServiceGuard_LockIsFreeDuringTheProbe(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true}
	g := fake.guard(t.TempDir())
	probing := make(chan struct{})
	release := make(chan struct{})
	g.probe = func(openCodeService) error {
		close(probing)
		<-release
		return errOpenCodeServiceSilent
	}

	attached := make(chan bool, 1)
	go func() {
		_, ok := g.attach("opencode")
		attached <- ok
	}()
	<-probing

	read := make(chan bool, 1)
	go func() {
		g.reportSilent()
		read <- g.stalled()
	}()
	select {
	case stalled := <-read:
		if !stalled {
			t.Fatal("reportSilent during the probe did not mark the service stalled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("guard readers blocked behind a hanging probe")
	}
	close(release)
	if ok := <-attached; ok {
		t.Fatal("attached a lane to a silent service")
	}
}

// A probe that answers while session requests report silence keeps the short gap: nothing hung on the lock.
func TestOpenCodeServiceGuard_RegisteredRequestSilenceKeepsTheShortGap(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0), registered: true}
	fake.answer.Store(true)
	g := fake.guard(t.TempDir())

	g.reportSilent()
	if _, ok := g.attach("opencode"); ok {
		t.Fatal("attached a lane while session requests report silence")
	}
	fake.advance(openCodeServiceRetryGap)
	g.attach("opencode")
	if got := fake.probes.Load(); got != 2 {
		t.Fatalf("health probes = %d after the retry gap, want 2: the answering probe is re-run at the short gap", got)
	}
	if got := fake.statuses.Load(); got != 0 {
		t.Fatalf("service status asked %d times, want 0", got)
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
		{name: "silent", reused: openCodeServiceStallBackoffBase},
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

// A service call goes to the healthy service, and otherwise to a standalone `opencode api`, never a bare one.
func TestRunOpenCodeServiceCall_TargetsSharedServiceOrStandalone(t *testing.T) {
	const password = "registration-secret"
	var removed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); !ok || pass != password {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete && r.URL.Path == "/api/session/ses_X" {
			removed.Add(1)
			fmt.Fprint(w, "true")
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
	if got := removed.Load(); got != 1 {
		t.Fatalf("service saw %d DELETE requests, want 1 while it answered", got)
	}
	logged, _ := os.ReadFile(calls)
	want := "api session.remove --param sessionID=ses_X --standalone pw=\n"
	if string(logged) != want {
		t.Fatalf("opencode api calls =\n%s\nwant\n%s", logged, want)
	}
}

// During a stall every standalone lookup ran to its 5s kill and held a status worker, so lookups keep the binding without one.
func TestQueryOpenCodeSession_V2SkipsStandaloneWhileServiceHangs(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	emptyPage := `{"data":[],"cursor":{"previous":null,"next":null}}`
	dir := setFakeOpenCodeSessionGet(t, emptyPage, movedSessionPayload("ses_BOUND", t.TempDir()))
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0)}
	prev := sharedOpenCodeService
	sharedOpenCodeService = fake.guard(t.TempDir())
	t.Cleanup(func() { sharedOpenCodeService = prev })

	inst := &Instance{Tool: "opencode", ProjectPath: t.TempDir(), OpenCodeSessionID: "ses_BOUND"}
	for range 3 {
		inst.updateOpenCodeSession(true)
		fake.advance(openCodeServiceRetryGap)
	}
	if inst.OpenCodeSessionID != "ses_BOUND" {
		t.Fatalf("binding = %q during the stall, want ses_BOUND kept", inst.OpenCodeSessionID)
	}
	if spawned, _ := filepath.Glob(filepath.Join(dir, "argv.*")); len(spawned) > 0 {
		t.Fatalf("started %d standalone opencode lookups while the service hung, want 0", len(spawned))
	}

	lookupFallsBack := func(phase string, wantCalls int) {
		t.Helper()
		// A stall is re-checked at the backoff base, the rest at the retry gap.
		fake.advance(openCodeServiceStallBackoffBase)
		if got := inst.queryOpenCodeSession(); got != "ses_BOUND" {
			t.Fatalf("%s: session ID = %q, want ses_BOUND from the standalone lookup", phase, got)
		}
		if got := fakeOpenCodeCallCount(t, dir); got != wantCalls {
			t.Fatalf("%s: opencode call count = %d, want %d", phase, got, wantCalls)
		}
	}
	stubStatus := sharedOpenCodeService.status

	// A check that fails for another reason, once the stall is past, falls back again.
	sharedOpenCodeService.status = func(string) (openCodeService, error) {
		return openCodeService{}, errors.New("opencode service status: timed out")
	}
	fake.advance(openCodeServiceFailureGap)
	lookupFallsBack("past the stall", 1)

	stallAgain := func(wantCalls int) {
		t.Helper()
		sharedOpenCodeService.status = stubStatus
		fake.advance(openCodeServiceRetryGap)
		inst.updateOpenCodeSession(true)
		if got := fakeOpenCodeCallCount(t, dir); got != wantCalls {
			t.Fatalf("silent again: opencode call count = %d, want %d", got, wantCalls)
		}
	}

	// A stopped or disabled service leaves nothing to hang on, so the lookup falls back inside the gap.
	stallAgain(1)
	fake.mu.Lock()
	fake.stopped = true
	fake.mu.Unlock()
	lookupFallsBack("stopped inside the gap", 2)

	stallAgain(2)
	sharedOpenCodeService.status = func(string) (openCodeService, error) { return openCodeService{}, errOpenCodeServiceDisabled }
	lookupFallsBack("disabled inside the gap", 3)
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
		{name: "disabled", pid: 0, disabled: true},
		// The setting gates starting, not attaching: a live registered process is waited on like any other.
		{name: "disabled, registered process alive", pid: os.Getpid(), disabled: true, wantWait: true},
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
				t.Fatalf("wait gave up after %d s, before a short stall clears (%s)", total, openCodeServiceStallGrace)
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
