package tmux

import (
	"testing"
	"time"
)

// Clears the cache by hand: invalidateCache would force the capture this helper detects.
func pollCaptured(t *testing.T, s *Session) (string, bool) {
	t.Helper()
	s.cacheMu.Lock()
	s.cacheContent = ""
	s.cacheTime = time.Time{}
	s.cacheMu.Unlock()
	status, err := s.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	s.cacheMu.RLock()
	captured := s.cacheContent != ""
	s.cacheMu.RUnlock()
	return status, captured
}

// An idle prompt pane prints nothing, so polls after the first should skip the capture.
func TestGetStatusSkipsCaptureForUnchangedPromptPane(t *testing.T) {
	prevSettle := promptSettleAge
	promptSettleAge = time.Second
	t.Cleanup(func() { promptSettleAge = prevSettle })

	cases := []struct {
		name         string
		acknowledged bool
		want         string
	}{
		{name: "waiting", want: "waiting"},
		{name: "idle", acknowledged: true, want: "idle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := startPaneWithContent(t, "idle-capture-gate-"+tc.name, realisticClaudeDoneContent, "claude", "Cooked for 32s")
			time.Sleep(2 * time.Second) // window_activity counts whole seconds, so the startup output settles first
			if tc.acknowledged {
				s.Acknowledge()
			}

			status, captured := pollCaptured(t, s)
			if status != tc.want || !captured {
				t.Fatalf("first poll = %q, captured=%v; want %q with a capture", status, captured, tc.want)
			}
			for poll := 2; poll <= 4; poll++ {
				status, captured = pollCaptured(t, s)
				if status != tc.want {
					t.Fatalf("poll %d = %q, want %q", poll, status, tc.want)
				}
				if captured {
					t.Fatalf("poll %d captured the pane again though it printed nothing since poll 1", poll)
				}
			}

			// Every send primitive calls invalidateCache, and a cached window_activity can predate the write.
			s.invalidateCache()
			if _, captured = pollCaptured(t, s); !captured {
				t.Fatal("poll after agent-deck wrote to the pane skipped the capture")
			}

			// Output agent-deck did not send must reach the capture once tmux reports it.
			if err := s.tmuxCmd("send-keys", "-t", s.Name, "x").Run(); err != nil {
				t.Fatalf("send keys: %v", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, captured = pollCaptured(t, s); captured {
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("pane printed new output but no poll captured it within 5s")
				}
				time.Sleep(200 * time.Millisecond)
			}
		})
	}
}
