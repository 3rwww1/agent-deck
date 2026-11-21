package tmux

import (
	"testing"
	"time"
)

// SetCachedSubstateForTest seeds the cached substate so tests can exercise
// layers that read it without classifying a real pane. It lives in a _test.go
// file so it never ships in the binary (issue #2986 item 6).
func (s *Session) SetCachedSubstateForTest(sub Substate, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSubstate, s.lastSubstateDetail = sub, detail
}

// ResetServerAliveCacheForTest forces the next IsServerAlive call to probe, now and after the test.
func ResetServerAliveCacheForTest(t testing.TB) {
	t.Helper()
	reset := func() {
		serverAliveMu.Lock()
		serverAliveVal = true
		serverAliveTime = time.Time{}
		serverAliveTimeouts = 0
		serverAliveMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}
