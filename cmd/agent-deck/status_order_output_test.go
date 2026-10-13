package main

import (
	"encoding/json"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func deadFirstInstances() []*session.Instance {
	statuses := []session.Status{session.StatusError, session.StatusStopped, session.StatusRunning, session.StatusIdle}
	instances := make([]*session.Instance, len(statuses))
	for i, s := range statuses {
		instances[i] = &session.Instance{ID: string(rune('a' + i)), Title: string(s), Tool: "shell", Status: s}
	}
	return instances
}

func TestBuildListJSONKeepsInputOrder(t *testing.T) {
	instances := deadFirstInstances()
	out, err := buildListJSON("_test", instances)
	if err != nil {
		t.Fatalf("buildListJSON: %v", err)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(rows) != len(instances) {
		t.Fatalf("got %d rows, want %d", len(rows), len(instances))
	}
	for i, inst := range instances {
		if rows[i].ID != inst.ID {
			t.Fatalf("row %d = %q, want %q", i, rows[i].ID, inst.ID)
		}
	}
}

func TestBuildChildRowsKeepsInputOrder(t *testing.T) {
	kids := deadFirstInstances()
	rows := buildChildRows(kids, cachedChildStatus)
	if len(rows) != len(kids) {
		t.Fatalf("got %d rows, want %d", len(rows), len(kids))
	}
	for i, k := range kids {
		if rows[i].ID != k.ID {
			t.Fatalf("row %d = %q, want %q", i, rows[i].ID, k.ID)
		}
	}
}
