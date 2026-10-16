package session

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// One guard per process: lanes share one health probe per TTL, and recovery runs under a file lock.

const (
	openCodeServiceHealthTTL     = 30 * time.Second
	openCodeServiceHealthTimeout = 2 * time.Second
	openCodeServiceActionTimeout = 30 * time.Second
	openCodeServiceLockName      = "opencode-service.lock"
)

type openCodeServiceGuard struct {
	mu        sync.Mutex
	svc       openCodeService
	healthyAt time.Time

	now     func() time.Time
	lockDir func() (string, error)
	status  func(binary string) (openCodeService, error)
	run     func(binary, action string) error
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

// attach is false while the service is unreachable, so no lane is pointed at a dead URL.
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

	if !errors.Is(err, errOpenCodeServiceStopped) {
		if err != nil {
			sessionLog.Debug("opencode_service_status_failed", slog.String("error", err.Error()))
		}
		return openCodeService{}, false
	}

	sessionLog.Warn("opencode_service_recover", slog.String("action", "start"))
	if err := g.run(binary, "start"); err != nil {
		sessionLog.Warn("opencode_service_recover_failed",
			slog.String("action", "start"), slog.String("error", err.Error()))
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
	return svc
}

// openCodeServiceLock serializes recovery across processes; a waiter re-reads the status first.
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
