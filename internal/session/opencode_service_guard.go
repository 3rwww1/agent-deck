package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/asheshgoplani/agent-deck/internal/childenv"
)

// Each owning 2.x TUI restarts a stalled service, so lanes attach to it and a stall is waited out: a restart drops every lane.

const (
	openCodeServiceHealthTTL  = 30 * time.Second
	openCodeServiceStallGrace = 2 * time.Minute
	openCodeServiceFailureGap = openCodeServiceStallGrace // lookups back off while the service hangs
	openCodeServiceRetryGap   = 5 * time.Second
	// A stall is re-probed at 15 s, 30 s, then every minute: one probe a minute is cheap, and a service that answers again is attached within it.
	openCodeServiceStallBackoffBase = 15 * time.Second
	openCodeServiceStallBackoffCap  = time.Minute
	openCodeServiceHealthTimeout    = 2 * time.Second
	openCodeServiceActionTimeout    = 30 * time.Second
	openCodeServiceLockName         = "opencode-service.lock"
	// Each poll of a waiting lane is one opencode process; the limit outlasts a short stall.
	openCodeServiceWaitPoll  = 5 * time.Second
	openCodeServiceWaitLimit = openCodeServiceStallGrace + time.Minute
)

// mu covers the recorded state only: a check runs its lock file, process listing, probe, and CLI with mu released.
type openCodeServiceGuard struct {
	mu              sync.Mutex
	checked         *sync.Cond // signals the end of the one check in flight
	checking        bool
	svc             openCodeService
	healthyAt       time.Time
	retryAt         time.Time
	lastFailedAt    time.Time
	requestSilentAt time.Time
	disabled        bool
	stalls          int          // stalled probes in a row, paces the backoff
	outages         atomic.Int64 // checks that found the service stopped or silent, read without waiting on a check in flight

	now        func() time.Time
	lockDir    func() (string, error)
	status     func(binary string) (openCodeService, error)
	run        func(binary, action string) error // "start"
	probe      func(svc openCodeService) error
	registered func() (openCodeService, error)
}

var sharedOpenCodeService = newOpenCodeServiceGuard()

func newOpenCodeServiceGuard() *openCodeServiceGuard {
	g := &openCodeServiceGuard{
		now:        time.Now,
		lockDir:    resolveLocksDirForSpawnLock,
		status:     openCodeServiceStatus,
		run:        runOpenCodeServiceAction,
		probe:      probeOpenCodeService,
		registered: openCodeRegisteredService,
	}
	g.checked = sync.NewCond(&g.mu)
	return g
}

// attach is false while the service is unreachable.
func (g *openCodeServiceGuard) attach(binary string) (openCodeService, bool) {
	g.mu.Lock()
	for {
		if !g.healthyAt.IsZero() && g.now().Sub(g.healthyAt) < openCodeServiceHealthTTL {
			g.mu.Unlock()
			return g.svc, true
		}
		if g.now().Before(g.retryAt) {
			g.mu.Unlock()
			return openCodeService{}, false
		}
		if !g.checking {
			break
		}
		if !g.lastFailedAt.IsZero() {
			// A lane that already knows the service stalled does not wait on the probe in flight.
			g.mu.Unlock()
			return openCodeService{}, false
		}
		// Lanes in one status sweep wait for the cold check in flight and share its verdict instead of each probing.
		g.checked.Wait()
	}
	g.checking = true
	g.mu.Unlock()

	svc, ok := g.checkOnce(binary)

	g.mu.Lock()
	if !ok {
		if retry := g.now().Add(openCodeServiceRetryGap); retry.After(g.retryAt) {
			g.retryAt = retry
		}
	}
	g.mu.Unlock()
	return svc, ok
}

// checkOnce clears the in-flight flag however check ends, so a panic never wedges every later attach.
func (g *openCodeServiceGuard) checkOnce(binary string) (openCodeService, bool) {
	defer func() {
		g.mu.Lock()
		g.checking = false
		g.checked.Broadcast()
		g.mu.Unlock()
	}()
	return g.record(g.check(binary))
}

// checkOutcome is what one check saw, recorded under mu once every lock, listing, probe, and CLI call is done.
type checkOutcome struct {
	svc          openCodeService
	err          error // the final verdict, nil for a service that answered
	disabled     bool
	stopped      bool // the first verdict was stopped, so a start was attempted
	started      bool // the start ran, and svc and err come from the probe after it
	probeStalled bool // the probe itself got no answer or no connection, apart from what session requests report
}

// check holds no guard lock: the lock file alone serializes recovery across processes.
func (g *openCodeServiceGuard) check(binary string) checkOutcome {
	lock, err := g.lock()
	if err != nil {
		sessionLog.Warn("opencode_service_lock_failed", slog.String("error", err.Error()))
		return checkOutcome{err: err}
	}
	defer lock.release()

	// A live registered service is probed straight from its file: `opencode service status` boots a runtime per call.
	svc, err := g.registered()
	if err != nil {
		// Only the CLI tells a stopped service from a disabled one, and starts it.
		svc, err = g.status(binary)
	}
	out := checkOutcome{svc: svc, err: err, disabled: errors.Is(err, errOpenCodeServiceDisabled)}
	if err == nil {
		out.err = g.probe(svc)
		out.probeStalled = isOpenCodeServiceStall(out.err)
		return out
	}
	if !errors.Is(err, errOpenCodeServiceStopped) {
		return out
	}
	out.stopped = true
	sessionLog.Warn("opencode_service_recover", slog.String("action", "start"))
	if err := g.run(binary, "start"); err != nil {
		sessionLog.Warn("opencode_service_recover_failed",
			slog.String("action", "start"), slog.String("error", err.Error()))
		return out
	}
	out.started = true
	if out.svc, out.err = g.status(binary); out.err == nil {
		out.err = g.probe(out.svc)
	}
	return out
}

func (g *openCodeServiceGuard) record(out checkOutcome) (openCodeService, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.disabled = out.disabled
	err := out.err
	if out.started {
		g.requestSilentAt = time.Time{}
	} else if err == nil && g.requestsRecentlySilent() {
		// A thrashing service can answer the cheap health route while session requests hang.
		err = errOpenCodeServiceSilent
	}
	// Only these drop the TUIs' connections; a rejected probe or a slow CLI leaves them be.
	if out.stopped || isOpenCodeServiceStall(err) {
		g.outages.Add(1)
	}
	// With no service left to hang on, an earlier silence no longer makes calls skip the standalone CLI.
	if out.stopped || g.disabled {
		g.lastFailedAt = time.Time{}
	}
	switch {
	case err == nil:
		return g.markHealthy(out.svc), true
	case out.stopped:
		g.stalls = 0
	case g.disabled:
		g.stalls = 0
		g.retryAt = g.now().Add(openCodeServiceHealthTTL)
	case !isOpenCodeServiceStall(err):
		// A service that rejects the probe (a 401, a 503 while it boots) is up and is retried at the short gap.
		g.stalls = 0
		sessionLog.Debug("opencode_service_check_failed", slog.String("error", err.Error()))
	default:
		// A stall clears by itself, and restarting the service drops every lane attached to it.
		if errors.Is(err, errOpenCodeServiceSilent) {
			// Only a hang gates the standalone CLI: a refused port says nothing about the host, so standalone calls keep running.
			g.lastFailedAt = g.now()
		}
		if !out.probeStalled {
			// The probe answered, so the backoff resets; only the session requests still hanging keep the short gap.
			g.stalls = 0
			break
		}
		g.stalls++
		g.retryAt = g.now().Add(openCodeServiceStallBackoff(g.stalls))
	}
	return openCodeService{}, false
}

// isOpenCodeServiceStall is true for a probe that hung or found no listener; both leave a live pid nothing can attach to.
func isOpenCodeServiceStall(err error) bool {
	return errors.Is(err, errOpenCodeServiceSilent) || errors.Is(err, errOpenCodeServiceUnreachable)
}

// openCodeServiceStallBackoff is the wait after the n-th stalled probe in a row.
func openCodeServiceStallBackoff(n int) time.Duration {
	wait := openCodeServiceStallBackoffBase
	for ; n > 1 && wait < openCodeServiceStallBackoffCap; n-- {
		wait *= 2
	}
	return min(wait, openCodeServiceStallBackoffCap)
}

// launchTarget also hands out a registered service that stopped answering, to wait on: a bare lane's own check would kill it.
func (g *openCodeServiceGuard) launchTarget(binary string) (svc openCodeService, wait, ok bool) {
	if svc, ok := g.attach(binary); ok {
		return svc, false, true
	}
	g.mu.Lock()
	disabled := g.disabled
	g.mu.Unlock()
	if disabled {
		return openCodeService{}, false, false
	}
	svc, err := g.registered()
	return svc, err == nil, err == nil
}

// reportSilent records a request to the checked service that got no answer in time.
func (g *openCodeServiceGuard) reportSilent() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.healthyAt = time.Time{}
	g.requestSilentAt = now
	g.lastFailedAt = now
}

func (g *openCodeServiceGuard) requestsRecentlySilent() bool {
	return !g.requestSilentAt.IsZero() && g.now().Sub(g.requestSilentAt) < openCodeServiceHealthTTL
}

// stalled is true while the service hung within the failure gap; a refused port backs off the probe without it.
func (g *openCodeServiceGuard) stalled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.lastFailedAt.IsZero() && g.now().Sub(g.lastFailedAt) < openCodeServiceFailureGap
}

func (g *openCodeServiceGuard) markHealthy(svc openCodeService) openCodeService {
	g.svc = svc
	g.healthyAt = g.now()
	g.retryAt = time.Time{}
	g.stalls = 0
	// An answered probe does not end a stall that session requests still report.
	if g.requestSilentAt.IsZero() || g.healthyAt.Sub(g.requestSilentAt) > openCodeServiceFailureGap {
		g.lastFailedAt = time.Time{}
		g.requestSilentAt = time.Time{}
	}
	return svc
}

// openCodeServiceLock serializes recovery across processes.
type openCodeServiceLock struct{ f *os.File }

func (g *openCodeServiceGuard) lock() (*openCodeServiceLock, error) {
	dir, err := g.lockDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, openCodeServiceLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &openCodeServiceLock{f: f}, nil
}

func (l *openCodeServiceLock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

func runOpenCodeServiceAction(binary, action string) error {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeServiceActionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "service", action)
	cmd.WaitDelay = 500 * time.Millisecond
	return cmd.Run()
}

var (
	errOpenCodeServiceSilent  = errors.New("opencode background service did not answer its health probe")
	errOpenCodeServiceStalled = errors.New("opencode background service is stalled")
	// A live pid whose port refuses is backed off like a stall, not restarted: `opencode service status` re-reads the same registration, so the CLI has nothing to add, and the owning TUI restarts it.
	errOpenCodeServiceUnreachable = errors.New("opencode background service refused its health probe")
)

// probeOpenCodeService makes the same probe an attached TUI makes before it starts.
func probeOpenCodeService(svc openCodeService) error {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeServiceHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.URL+"/api/info", nil)
	if err != nil {
		return err
	}
	if svc.Password != "" {
		req.SetBasicAuth("opencode", svc.Password)
	}
	resp, err := openCodeSessionHTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return errOpenCodeServiceSilent
		}
		return fmt.Errorf("%w: %w", errOpenCodeServiceUnreachable, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode service health probe: HTTP %s", resp.Status)
	}
	return nil
}

// attachOpenCodeToSharedService leaves the password to the pane's shell, out of the stored command.
func (i *Instance) attachOpenCodeToSharedService(cmd string) string {
	binary, ok := i.openCodeLaunchBinary()
	if !ok {
		return cmd
	}
	svc, wait, ok := sharedOpenCodeService.launchTarget(binary)
	if !ok {
		return cmd
	}
	attached := cmd + " --server " + shellescape.Quote(svc.URL)
	if svc.Password != "" {
		attached = `OPENCODE_PASSWORD="$(` + cmd + ` service get password)" ` + attached
	}
	if wait {
		// `service status` prints the URL once the service answers; past the limit OpenCode reports it unreachable.
		attached = fmt.Sprintf(`waited=0 && until %s service status 2>/dev/null | grep -q '^http' || [ "$waited" -ge %d ]; do waited=$((waited+1)); sleep %d; done && `,
			cmd, openCodeServiceWaitLimit/openCodeServiceWaitPoll, int(openCodeServiceWaitPoll/time.Second)) + attached
	}
	return attached
}

// openCodeAPICommand never makes a bare api call: its service check kills a service it finds stalled.
func openCodeAPICommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	svc, ok := sharedOpenCodeService.attach(binary)
	target := []string{"--standalone"}
	if ok {
		target = []string{"--server", svc.URL}
	}
	argv := slices.Concat([]string{"api"}, args, target)
	// A configured launcher may carry environment assignments before the binary.
	cmd, err := (&Instance{Command: binary}).openCodeCLICommand(ctx, "", argv...)
	if err != nil {
		// #nosec G204 -- binary is the resolved opencode binary and agent-deck builds every argument; no shell.
		cmd = exec.CommandContext(ctx, binary, argv...)
		cmd.Env = childenv.ForLaunch("")
	}
	if svc.Password != "" {
		cmd.Env = append(cmd.Env, "OPENCODE_PASSWORD="+svc.Password)
	}
	return cmd
}
