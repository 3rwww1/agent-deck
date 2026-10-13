package tmux

import (
	"sync"
	"sync/atomic"
	"time"
)

// cacheRefreshGate turns a stale shared cache into one refresh per TTL for
// every per-row reader, instead of one direct tmux probe per row.
//
// It stays shut until the process has refreshed the cache once: a one-shot
// command that never warmed a cache keeps its single direct probe rather than
// paying a fleet-wide listing. A failed refresh shuts it for one TTL, so a
// dead server costs one bounded listing per TTL, not one per row.
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
