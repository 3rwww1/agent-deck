package core

import (
	"context"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestLiveSessionRowsKeepInputOrder(t *testing.T) {
	statuses := []session.Status{session.StatusError, session.StatusStopped, session.StatusRunning, session.StatusError, session.StatusIdle}
	instances := make([]*session.Instance, len(statuses))
	cached := make(map[*session.Instance]bool, len(statuses))
	for i, s := range statuses {
		instances[i] = &session.Instance{ID: string(rune('a' + i)), Title: string(s), Status: s}
		cached[instances[i]] = true
	}
	rows := liveSessionRows(context.Background(), "_test", instances, cached, nil)
	if len(rows) != len(instances) {
		t.Fatalf("got %d rows, want %d", len(rows), len(instances))
	}
	for i, inst := range instances {
		if rows[i].ID != inst.ID || rows[i].Status != string(inst.Status) {
			t.Fatalf("row %d = %s/%s, want %s/%s", i, rows[i].ID, rows[i].Status, inst.ID, inst.Status)
		}
	}
}
