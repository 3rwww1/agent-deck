package session

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// A TUI that lost its session on a reconnect sits on its home screen: the lane is reopened once per disruption.
func TestOpenCodeRouteWatch_ReopensLaneThatLostItsSessionOnReconnect(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/info":
			fmt.Fprint(w, `{"version":"2.0.25"}`)
		case "/api/session/ses_TITLED":
			fmt.Fprint(w, `{"data":{"id":"ses_TITLED","title":"Fix the login page"}}`)
		case "/api/session/ses_UNTITLED":
			fmt.Fprint(w, `{"data":{"id":"ses_UNTITLED","title":"New session - 2026-10-14T20:21:00.000Z"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	useOpenCodeStub(t, fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = 'service status' ] && echo %s\n", srv.URL))
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	restartService := func(pid int) {
		t.Helper()
		if err := os.MkdirAll(stateHome+"/opencode", 0o700); err != nil {
			t.Fatal(err)
		}
		reg := fmt.Sprintf(`{"url":%q,"pid":%d,"password":""}`, srv.URL, pid)
		if err := os.WriteFile(stateHome+"/opencode/service.json", []byte(reg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	guard := useFreshOpenCodeServiceGuard(t)

	started := time.Now().Add(-2 * openCodeRouteSettle)
	lane := func(id, sessionID string) *Instance {
		return &Instance{ID: id, Tool: "opencode", OpenCodeSessionID: sessionID, lastStartTime: started}
	}
	titled, untitled, fresh := lane("titled", "ses_TITLED"), lane("untitled", "ses_UNTITLED"), lane("fresh", "ses_TITLED")
	fresh.lastStartTime = time.Now()
	lanes := []*Instance{titled, untitled, fresh}

	titles := map[string]string{}
	reopened := map[string]int{}
	w := newOpenCodeRouteWatch()
	w.pane = func(inst *Instance) (tmux.PaneInfo, bool) { return tmux.PaneInfo{Title: titles[inst.ID]}, true }
	w.reopen = func(inst *Instance) error { reopened[inst.ID]++; return nil }
	show := func(title string) {
		for _, inst := range lanes {
			titles[inst.ID] = title
		}
	}
	sweep := func(step string, want map[string]int) {
		t.Helper()
		w.sweep(lanes)
		for _, inst := range lanes {
			if reopened[inst.ID] != want[inst.ID] {
				t.Fatalf("%s: lane %s reopened %d times, want %d", step, inst.ID, reopened[inst.ID], want[inst.ID])
			}
		}
	}

	restartService(os.Getpid())
	show("OC | Fix the login page")
	sweep("lanes on their sessions", nil)
	show(openCodeHomeTitle)
	sweep("user went home", nil)
	sweep("user stays home", nil)

	show("OC | Fix the login page")
	sweep("back on their sessions", nil)
	restartService(os.Getppid())
	show(openCodeHomeTitle)
	sweep("home once after a service restart", nil)
	sweep("home twice after a service restart", map[string]int{"titled": 1})
	sweep("still home under the same service", map[string]int{"titled": 1})

	show("OC | Fix the login page")
	sweep("back on their sessions after the reopen", map[string]int{"titled": 1})
	guard.outages.Add(1)
	show(openCodeHomeTitle)
	sweep("home once after a failed check, same service", map[string]int{"titled": 1})
	sweep("home twice after a failed check, same service", map[string]int{"titled": 2})

	if err := os.Remove(stateHome + "/opencode/service.json"); err != nil {
		t.Fatal(err)
	}
	show("OC | Fix the login page")
	sweep("service gone", map[string]int{"titled": 2})
	show(openCodeHomeTitle)
	sweep("home with no service, once", map[string]int{"titled": 2})
	sweep("home with no service, twice", map[string]int{"titled": 2})
}
