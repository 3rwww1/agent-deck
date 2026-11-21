package session

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stubOpenCodeService struct {
	mu     sync.Mutex
	now    time.Time
	answer atomic.Bool
	probes atomic.Int32
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

// guard is one agent-deck process.
func (f *stubOpenCodeService) guard() *openCodeServiceGuard {
	g := newOpenCodeServiceGuard()
	g.now = f.clock
	g.status = func(string) (openCodeService, error) {
		return openCodeService{URL: "http://127.0.0.1:49374", Password: "secret"}, nil
	}
	g.healthy = func(openCodeService) bool {
		f.probes.Add(1)
		return f.answer.Load()
	}
	return g
}

// A silent service is never cached; a healthy one is probed once per TTL, however many lanes attach.
func TestOpenCodeServiceGuard_HealthyServiceIsProbedOncePerTTL(t *testing.T) {
	fake := &stubOpenCodeService{now: time.Unix(1_000_000, 0)}
	g := fake.guard()

	if _, ok := g.attach("opencode"); ok {
		t.Fatal("attached a lane to a silent service")
	}
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

// useFreshOpenCodeServiceGuard swaps in a new process-wide guard for the test.
func useFreshOpenCodeServiceGuard(t *testing.T) *openCodeServiceGuard {
	t.Helper()
	prev := sharedOpenCodeService
	sharedOpenCodeService = newOpenCodeServiceGuard()
	t.Cleanup(func() { sharedOpenCodeService = prev })
	return sharedOpenCodeService
}
