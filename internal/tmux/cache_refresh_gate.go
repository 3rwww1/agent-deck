package tmux

import (
	"sync"
	"sync/atomic"
	"time"
)

// cacheRefreshGate gives per-row readers one shared refresh per TTL, only after a first refresh in the process.
type cacheRefreshGate struct {
	ttl   time.Duration
	stale func() bool

	mu       sync.Mutex
	warm     atomic.Bool
	failedAt atomic.Int64
}

func (g *cacheRefreshGate) markWarm() { g.warm.Store(true) }

// cacheRefreshEpoch keeps failedAt monotonic, so a wall-clock step cannot stretch the backoff. Zero means never failed.
var cacheRefreshEpoch = time.Now()

func (g *cacheRefreshGate) markFailed() { g.failedAt.Store(int64(time.Since(cacheRefreshEpoch)) + 1) }

func (g *cacheRefreshGate) reset() {
	g.warm.Store(false)
	g.failedAt.Store(0)
}

// refreshIfStale takes the refresher as an argument because the refresher marks the gate.
func (g *cacheRefreshGate) refreshIfStale(refresh func()) {
	if !g.warm.Load() || g.backingOff() || !g.stale() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.backingOff() || !g.stale() {
		return
	}
	refresh()
}

func (g *cacheRefreshGate) backingOff() bool {
	failedAt := g.failedAt.Load()
	return failedAt != 0 && time.Since(cacheRefreshEpoch)-time.Duration(failedAt) < g.ttl
}
