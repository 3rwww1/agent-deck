package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/sendqueue"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestWatchOpenCodeLanded_SettlesWithWhatTheSessionShowed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Room for a second check, so a transient failure retries and a removed session does not.
	t.Setenv("AGENTDECK_SEND_LAND_WINDOW", "1100ms")
	unreadable := fmt.Errorf("%w: HTTP 404", session.ErrOpenCodeMessagesUnreadable)
	for _, tc := range []struct {
		name             string
		id               string
		err              error
		wantRow, wantWhy string
		wantChecks       int
		wantLanded       bool
	}{
		{name: "landed", id: "msg_1", wantRow: "msg_1", wantChecks: 1, wantLanded: true},
		{name: "not there yet", wantWhy: "not seen in the OpenCode session within 1.1s", wantChecks: 2},
		{name: "service down", err: errors.New("shared opencode service unavailable"),
			wantWhy: "could not confirm landing: shared opencode service unavailable", wantChecks: 2},
		{name: "session gone", err: unreadable,
			wantWhy: "could not confirm landing: " + unreadable.Error(), wantChecks: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := 0
			prev := findOpenCodeLanded
			findOpenCodeLanded = func(_ *session.Instance, text string, _ time.Time) (string, string, error) {
				checks++
				if text != "continue" {
					t.Fatalf("checked for %q", text)
				}
				return tc.id, "2026-01-01T00:00:00Z", tc.err
			}
			t.Cleanup(func() { findOpenCodeLanded = prev })

			rec := &sendqueue.Record{SessionID: "lane", Tool: "opencode", Message: "continue", SentAt: time.Now().Format(time.RFC3339Nano)}
			watchOpenCodeLanded("", &session.Instance{Tool: "opencode"}, rec, func(f func(*sendqueue.Record)) error { f(rec); return nil })
			if landed := rec.State == sendqueue.StateLanded; landed != tc.wantLanded || rec.LandedRowID != tc.wantRow || rec.Reason != tc.wantWhy || checks != tc.wantChecks {
				t.Fatalf("record = state %q row %q reason %q after %d checks", rec.State, rec.LandedRowID, rec.Reason, checks)
			}
			if !tc.wantLanded && !rec.Settled {
				t.Fatal("record left unsettled")
			}
		})
	}
}
