package session

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// Each owning 2.x TUI restarts a stalled service, so lanes attach and only agent-deck recovers it.

const (
	openCodeServiceHealthTTL     = 30 * time.Second
	openCodeServiceStallGrace    = 2 * time.Minute
	openCodeServiceFailureGap    = 45 * time.Second
	openCodeServiceHealthTimeout = 2 * time.Second
	openCodeServiceActionTimeout = 30 * time.Second
	openCodeServiceLockName      = "opencode-service.lock"
)

type openCodeServiceGuard struct {
	mu             sync.Mutex
	svc            openCodeService
	healthyAt      time.Time
	unhealthySince time.Time
	lastFailedAt   time.Time

	now     func() time.Time
	lockDir func() (string, error)
	status  func(binary string) (openCodeService, error)
	run     func(binary, action string) error // "start" or "restart"
	healthy func(svc openCodeService) bool
}

var sharedOpenCodeService = newOpenCodeServiceGuard()

func newOpenCodeServiceGuard() *openCodeServiceGuard {
	return &openCodeServiceGuard{
		now:     time.Now,
		lockDir: resolveLocksDirForSpawnLock,
		status:  openCodeServiceStatus,
		run:     runOpenCodeServiceAction,
		healthy: openCodeServiceHealthy,
	}
}

// attach is false while the service is unreachable, so the lane launches the default way instead.
func (g *openCodeServiceGuard) attach(binary string) (openCodeService, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.healthyAt.IsZero() && g.now().Sub(g.healthyAt) < openCodeServiceHealthTTL {
		return g.svc, true
	}

	lock, err := g.lock()
	if err != nil {
		sessionLog.Warn("opencode_service_lock_failed", slog.String("error", err.Error()))
		return openCodeService{}, false
	}
	defer lock.release()

	svc, err := g.status(binary)
	if err == nil && g.healthy(svc) {
		return g.markHealthy(svc), true
	}

	var action string
	switch {
	case errors.Is(err, errOpenCodeServiceStopped):
		action = "start"
	case err != nil:
		sessionLog.Debug("opencode_service_status_failed", slog.String("error", err.Error()))
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
	if svc, err := g.status(binary); err == nil && g.healthy(svc) {
		return g.markHealthy(svc), true
	}
	return openCodeService{}, false
}

func (g *openCodeServiceGuard) markHealthy(svc openCodeService) openCodeService {
	g.svc = svc
	g.healthyAt = g.now()
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

// openCodeServiceHealthy makes the same probe an attached TUI makes before it starts.
func openCodeServiceHealthy(svc openCodeService) bool {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeServiceHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.URL+"/api/info", nil)
	if err != nil {
		return false
	}
	if svc.Password != "" {
		req.SetBasicAuth("opencode", svc.Password)
	}
	resp, err := openCodeSessionHTTPClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// attachOpenCodeToSharedService leaves the password to the pane's shell, out of the stored command.
func (i *Instance) attachOpenCodeToSharedService(cmd string) string {
	binary, ok := i.openCodeLaunchBinary()
	if !ok {
		return cmd
	}
	svc, ok := sharedOpenCodeService.attach(binary)
	if !ok {
		return cmd
	}
	attached := cmd + " --server " + shellescape.Quote(svc.URL)
	if svc.Password == "" {
		return attached
	}
	return `OPENCODE_PASSWORD="$(` + cmd + ` service get password)" ` + attached
}
