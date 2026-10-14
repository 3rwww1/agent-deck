package main

import (
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestCheckOpenCodeSessionConflict_RefusesIDHeldByAnotherSession(t *testing.T) {
	holder := &session.Instance{ID: "holder-id", Title: "holder", OpenCodeSessionID: "ses_X"}
	lane := &session.Instance{ID: "lane-id", Title: "lane"}
	msg, code := checkOpenCodeSessionConflict([]*session.Instance{holder, lane}, lane, " ses_X ")
	if code != ErrCodeAlreadyExists || !strings.Contains(msg, "holder-id") {
		t.Fatalf("got (%q, %q); want ALREADY_EXISTS naming holder-id", msg, code)
	}
}

func TestCheckOpenCodeSessionConflict_AllowsOwnIDAndClear(t *testing.T) {
	lane := &session.Instance{ID: "lane-id", Title: "lane", OpenCodeSessionID: "ses_X"}
	other := &session.Instance{ID: "other-id", Title: "other", OpenCodeSessionID: "ses_Y"}
	for _, value := range []string{"ses_X", "", "ses_Z"} {
		if msg, _ := checkOpenCodeSessionConflict([]*session.Instance{lane, other}, lane, value); msg != "" {
			t.Fatalf("value %q refused: %s", value, msg)
		}
	}
}
