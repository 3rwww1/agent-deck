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

func (g *cacheRefreshGate) markFailed() { g.failedAt.Store(time.Now().UnixNano()) }

func (g *cacheRefreshGate) reset() {
	g.warm.Store(false)
	g.failedAt.Store(0)
}

// refreshIfStale takes the refresher as an argument because the refresher marks the gate.
// Concurrent readers queue behind one refresh, which is cheaper than each probing.
func (g *cacheRefreshGate) refreshIfStale(refresh func()) {
	if !g.warm.Load() || !g.stale() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.stale() || time.Since(time.Unix(0, g.failedAt.Load())) < g.ttl {
		return
	}
	refresh()
}
