package session

import (
	"context"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

func wantedPipeReviver(t *testing.T, want func(string) bool, revived *int) *Reviver {
	t.Helper()
	prev := tmux.GetPipeManager()
	pm := tmux.NewPipeManager(context.Background(), nil)
	pm.SetWantPipe(want)
	tmux.SetPipeManager(pm)
	t.Cleanup(func() {
		tmux.SetPipeManager(prev)
		pm.Close()
	})

	r := NewReviver()
	r.TmuxExists = func(string, string) bool { return true }
	r.ReviveAction = func(*Instance) error { *revived++; return nil }
	r.Breaker = NewReviveBreaker(nil)
	r.AuthHeld = nil
	r.Log = nil
	return r
}

// Only attached and recently focused sessions hold pipes; a missing pipe elsewhere is expected.
func TestReviver_DeadPipeCountsOnlyWhenWanted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  Status
		wanted  bool
		class   RevivalClass
		revived int
	}{
		{"unwanted running session stays alive", StatusRunning, false, ClassAlive, 0},
		{"wanted session with a dead pipe is revived", StatusWaiting, true, ClassErrored, 1},
		{"stored error is revived even when unwanted", StatusError, false, ClassErrored, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revived := 0
			r := wantedPipeReviver(t, func(string) bool { return tc.wanted }, &revived)
			r.ConfirmAfter = 0

			out := r.ReviveAll([]*Instance{newReviverTestInstance("wanted-pipe", tc.status)})

			if out[0].Class != tc.class {
				t.Errorf("class = %v, want %v", out[0].Class, tc.class)
			}
			if revived != tc.revived {
				t.Errorf("ReviveAction called %d times, want %d", revived, tc.revived)
			}
		})
	}
}

// Focus moving away during the confirm wait leaves nothing for Connect to do.
func TestReviver_ConfirmDropsPipeNoLongerWanted(t *testing.T) {
	wanted := true
	revived := 0
	r := wantedPipeReviver(t, func(string) bool { return wanted }, &revived)
	r.sleep = func(time.Duration) { wanted = false }

	out := r.ReviveAll([]*Instance{newReviverTestInstance("focus-moved", StatusWaiting)})

	if out[0].Class != ClassAlive {
		t.Errorf("class = %v, want %v", out[0].Class, ClassAlive)
	}
	if revived != 0 {
		t.Errorf("ReviveAction called %d times, want 0", revived)
	}
}
