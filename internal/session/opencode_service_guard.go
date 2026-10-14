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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/asheshgoplani/agent-deck/internal/childenv"
)

// Each owning 2.x TUI restarts a stalled service, so lanes attach and only agent-deck recovers it.

const (
	openCodeServiceHealthTTL     = 30 * time.Second
	openCodeServiceStallGrace    = 2 * time.Minute
	openCodeServiceFailureGap    = 45 * time.Second
	openCodeServiceRetryGap      = 5 * time.Second
	openCodeServiceHealthTimeout = 2 * time.Second
	openCodeServiceActionTimeout = 30 * time.Second
	openCodeServiceLockName      = "opencode-service.lock"
	// Each poll of a waiting lane is one opencode process; the limit leaves room for one stall restart.
	openCodeServiceWaitPoll  = 5 * time.Second
	openCodeServiceWaitLimit = openCodeServiceStallGrace + time.Minute
)

type openCodeServiceGuard struct {
	mu             sync.Mutex
	svc            openCodeService
	healthyAt      time.Time
	retryAt        time.Time
	unhealthySince time.Time
	lastFailedAt   time.Time
	disabled       bool
	outages        atomic.Int64 // failed checks, read without waiting on a check in flight

	now        func() time.Time
	lockDir    func() (string, error)
	status     func(binary string) (openCodeService, error)
	run        func(binary, action string) error // "start" or "restart"
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
	if !g.disabled {
		g.outages.Add(1)
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

	svc, err := g.status(binary)
	g.disabled = errors.Is(err, errOpenCodeServiceDisabled)
	if err == nil {
		if err = g.probe(svc); err == nil {
			return g.markHealthy(svc), true
		}
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
		// A stall usually clears by itself; failures further apart than the gap are separate outages.
		now := g.now()
		if g.unhealthySince.IsZero() || now.Sub(g.lastFailedAt) > openCodeServiceFailureGap {
			g.unhealthySince = now
		}
		g.lastFailedAt = now
		if now.Sub(g.unhealthySince) < openCodeServiceStallGrace {
			return openCodeService{}, false
		}
		// Another agent-deck process may have restarted it moments ago.
		if last := lock.lastRestart(); !last.IsZero() && now.Sub(last) < openCodeServiceStallGrace {
			return openCodeService{}, false
		}
		lock.recordRestart(now)
		action = "restart"
	}

	sessionLog.Warn("opencode_service_recover", slog.String("action", action))
	if err := g.run(binary, action); err != nil {
		sessionLog.Warn("opencode_service_recover_failed",
			slog.String("action", action), slog.String("error", err.Error()))
		return openCodeService{}, false
	}
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

func (g *openCodeServiceGuard) markHealthy(svc openCodeService) openCodeService {
	g.svc = svc
	g.healthyAt = g.now()
	g.retryAt = time.Time{}
	g.unhealthySince = time.Time{}
	g.lastFailedAt = time.Time{}
	return svc
}

// openCodeServiceLock serializes recovery across processes and holds the last restart time.
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

func (l *openCodeServiceLock) lastRestart() time.Time {
	buf := make([]byte, 32)
	n, _ := l.f.ReadAt(buf, 0)
	unix, err := strconv.ParseInt(strings.TrimSpace(string(buf[:n])), 10, 64)
	if err != nil || unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

func (l *openCodeServiceLock) recordRestart(at time.Time) {
	_ = l.f.Truncate(0)
	_, _ = l.f.WriteAt([]byte(strconv.FormatInt(at.Unix(), 10)), 0)
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

var errOpenCodeServiceSilent = errors.New("opencode background service did not answer its health probe")

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
	// #nosec G204 -- binary is the resolved opencode binary and agent-deck builds every argument; no shell.
	cmd := exec.CommandContext(ctx, binary, slices.Concat([]string{"api"}, args, target)...)
	if svc.Password != "" {
		cmd.Env = append(childenv.ForLaunch(""), "OPENCODE_PASSWORD="+svc.Password)
	}
	return cmd
}
