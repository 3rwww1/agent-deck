package session

import "testing"

// A sweep walks every owned row, most of them stopped, so a tmux spawn per row adds up.
func TestReviverSweepListsEachSocketOnce(t *testing.T) {
	sock := livenessSocket(t)
	startLivenessSession(t, sock, "live-0")
	rows := append(deadRows(sock, 300, StatusStopped), livenessRow("live-0", sock, StatusRunning))
	// Classify probes the stored socket, which livenessRow leaves on the default server.
	for _, row := range rows {
		row.TmuxSocketName = sock
	}

	r := NewReviver()
	r.PipeAlive = func(string) bool { return true }
	r.ReviveAction = func(inst *Instance) error {
		t.Errorf("%s was revived", inst.Title)
		return nil
	}
	r.Breaker = nil
	r.AuthHeld = nil
	r.ConfirmAfter = 0
	r.Log = nil

	var out []ReviveOutcome
	got := spawns(sock, func() { out = r.ReviveAll(rows) })

	for _, o := range out {
		want := ClassDead
		if o.Title == "live-0" {
			want = ClassAlive
		}
		if o.Class != want {
			t.Errorf("%s: class = %v, want %v", o.Title, o.Class, want)
		}
	}
	if got > 3 {
		t.Fatalf("sweep of %d rows on one socket spawned %d tmux processes, want at most 3", len(rows), got)
	}
}
