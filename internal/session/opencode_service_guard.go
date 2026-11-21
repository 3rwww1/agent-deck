package session

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// One guard per agent-deck process, so a dozen lanes cost one health probe per TTL.

const (
	openCodeServiceHealthTTL     = 30 * time.Second
	openCodeServiceHealthTimeout = 2 * time.Second
)

type openCodeServiceGuard struct {
	mu        sync.Mutex
	svc       openCodeService
	healthyAt time.Time

	now     func() time.Time
	status  func(binary string) (openCodeService, error)
	healthy func(svc openCodeService) bool
}

var sharedOpenCodeService = newOpenCodeServiceGuard()

func newOpenCodeServiceGuard() *openCodeServiceGuard {
	return &openCodeServiceGuard{
		now:     time.Now,
		status:  openCodeServiceStatus,
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
	svc, err := g.status(binary)
	if err != nil || !g.healthy(svc) {
		return openCodeService{}, false
	}
	return g.markHealthy(svc), true
}

func (g *openCodeServiceGuard) markHealthy(svc openCodeService) openCodeService {
	g.svc = svc
	g.healthyAt = g.now()
	return svc
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
