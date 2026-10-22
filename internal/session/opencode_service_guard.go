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
	// A registered service that stops answering is probed over HTTP only, so the stall is paced slower than the CLI path.
	openCodeServiceStallGap      = 30 * time.Second
	openCodeServiceHealthTimeout = 2 * time.Second
	openCodeServiceActionTimeout = 30 * time.Second
	openCodeServiceLockName      = "opencode-service.lock"
	// Each poll of a waiting lane is one opencode process; the limit outlasts a short stall.
	openCodeServiceWaitPoll  = 5 * time.Second
	openCodeServiceWaitLimit = openCodeServiceStallGrace + time.Minute
)

type openCodeServiceGuard struct {
	mu              sync.Mutex
	svc             openCodeService
	healthyAt       time.Time
	retryAt         time.Time
	lastFailedAt    time.Time
	requestSilentAt time.Time
	disabled        bool
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
	return &openCodeServiceGuard{
		now:        time.Now,
		lockDir:    resolveLocksDirForSpawnLock,
		status:     openCodeServiceStatus,
		run:        runOpenCodeServiceAction,
		probe:      probeOpenCodeService,
		registered: openCodeRegisteredService,
	}
}

// attach is false while the service is unreachable.
func (g *openCodeServiceGuard) attach(binary string) (openCodeService, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.healthyAt.IsZero() && g.now().Sub(g.healthyAt) < openCodeServiceHealthTTL {
		return g.svc, true
	}
	if g.now().Before(g.retryAt) {
		return openCodeService{}, false
	}
	if svc, ok := g.check(binary); ok {
		return svc, true
	}
	// Lanes in one status sweep share a failed verdict instead of each probing.
	if retry := g.now().Add(openCodeServiceRetryGap); retry.After(g.retryAt) {
		g.retryAt = retry
	}
	return openCodeService{}, false
}

func (g *openCodeServiceGuard) check(binary string) (openCodeService, bool) {
	lock, err := g.lock()
	if err != nil {
		sessionLog.Warn("opencode_service_lock_failed", slog.String("error", err.Error()))
		return openCodeService{}, false
	}
	defer lock.release()

	// A live registered service is probed straight from its file: `opencode service status` boots a runtime per call.
	svc, err := g.registered()
	viaRegistration := err == nil
	if !viaRegistration {
		// Only the CLI tells a stopped service from a disabled one, and starts it.
		svc, err = g.status(binary)
	}
	g.disabled = errors.Is(err, errOpenCodeServiceDisabled)
	probeSilent := false
	if err == nil {
		err = g.probe(svc)
		probeSilent = errors.Is(err, errOpenCodeServiceSilent)
		if err == nil && g.requestsRecentlySilent() {
			// A thrashing service can answer the cheap health route while session requests hang.
			err = errOpenCodeServiceSilent
		}
		if err == nil {
			return g.markHealthy(svc), true
		}
	}
	// Only these drop the TUIs' connections; a rejected probe or a slow CLI leaves them be.
	if errors.Is(err, errOpenCodeServiceStopped) || errors.Is(err, errOpenCodeServiceSilent) {
		g.outages.Add(1)
	}
	// With no service left to hang on, an earlier silence no longer makes calls skip the standalone CLI.
	if errors.Is(err, errOpenCodeServiceStopped) || g.disabled {
		g.lastFailedAt = time.Time{}
	}

	var action string
	switch {
	case errors.Is(err, errOpenCodeServiceStopped):
		action = "start"
	case errors.Is(err, errOpenCodeServiceDisabled):
		g.retryAt = g.now().Add(openCodeServiceHealthTTL)
		return openCodeService{}, false
	case !errors.Is(err, errOpenCodeServiceSilent):
		// Only silence is a stall: a service that rejects the probe (a 401, a 503 while it boots) is up.
		sessionLog.Debug("opencode_service_check_failed", slog.String("error", err.Error()))
		return openCodeService{}, false
	default:
		// A stall clears by itself, and restarting the service drops every lane attached to it.
		g.lastFailedAt = g.now()
		if viaRegistration && probeSilent {
			// Without the CLI only the probe paces the stall; a probe that answered keeps the short gap.
			g.retryAt = g.now().Add(openCodeServiceStallGap)
		}
		return openCodeService{}, false
	}

	sessionLog.Warn("opencode_service_recover", slog.String("action", action))
	if err := g.run(binary, action); err != nil {
		sessionLog.Warn("opencode_service_recover_failed",
			slog.String("action", action), slog.String("error", err.Error()))
		return openCodeService{}, false
	}
	g.requestSilentAt = time.Time{}
	if svc, err := g.status(binary); err == nil && g.probe(svc) == nil {
		return g.markHealthy(svc), true
	}
	return openCodeService{}, false
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

// stalled is true while the service went silent within the failure gap.
func (g *openCodeServiceGuard) stalled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.lastFailedAt.IsZero() && g.now().Sub(g.lastFailedAt) < openCodeServiceFailureGap
}

func (g *openCodeServiceGuard) markHealthy(svc openCodeService) openCodeService {
	g.svc = svc
	g.healthyAt = g.now()
	g.retryAt = time.Time{}
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
		return err
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
