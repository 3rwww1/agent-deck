package tmux

import (
	"testing"
	"time"
)

func TestRetireSettledActivityWaitsOutCaptureLag(t *testing.T) {
	const activityTS = 1_000_000
	activityStart := time.Unix(activityTS, 0)
	cases := []struct {
		name       string
		capturedAt time.Time
		retired    bool
	}{
		{name: "same second as the activity", capturedAt: activityStart.Add(400 * time.Millisecond)},
		{name: "just short of the settle age", capturedAt: activityStart.Add(promptSettleAge - time.Millisecond)},
		{name: "at the settle age", capturedAt: activityStart.Add(promptSettleAge), retired: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{stateTracker: &StateTracker{lastActivityTimestamp: activityTS - 60}}
			s.retireSettledActivityLocked(activityTS, tc.capturedAt)
			if retired := s.stateTracker.lastActivityTimestamp == activityTS; retired != tc.retired {
				t.Fatalf("retired = %v, want %v", retired, tc.retired)
			}
		})
	}
}
