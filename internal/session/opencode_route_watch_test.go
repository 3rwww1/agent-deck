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

// A TUI that lost its session on a reconnect sits on its home screen: the lane is reopened once per disruption, unless someone is looking at it.
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
	titled, untitled, fresh, watched := lane("titled", "ses_TITLED"), lane("untitled", "ses_UNTITLED"), lane("fresh", "ses_TITLED"), lane("watched", "ses_TITLED")
	fresh.lastStartTime = time.Now()
	lanes := []*Instance{titled, untitled, fresh, watched}

	titles := map[string]string{}
	reopened := map[string]int{}
	w := newOpenCodeRouteWatch()
	w.pane = func(inst *Instance) (tmux.PaneInfo, bool) { return tmux.PaneInfo{Title: titles[inst.ID]}, true }
	attached := map[string]bool{"watched": true}
	w.watched = func(inst *Instance) bool { return attached[inst.ID] }
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
	attached["watched"] = false
	sweep("still home under the same service, watcher gone", map[string]int{"titled": 1})

	show("OC | Fix the login page")
	sweep("back on their sessions after the reopen", map[string]int{"titled": 1})
	guard.outages.Add(1)
	show(openCodeHomeTitle)
	sweep("home once after a failed check, same service", map[string]int{"titled": 1})
	sweep("home twice after a failed check, same service", map[string]int{"titled": 2, "watched": 1})

	if err := os.Remove(stateHome + "/opencode/service.json"); err != nil {
		t.Fatal(err)
	}
	show("OC | Fix the login page")
	sweep("service gone", map[string]int{"titled": 2, "watched": 1})
	show(openCodeHomeTitle)
	sweep("home with no service, once", map[string]int{"titled": 2, "watched": 1})
	sweep("home with no service, twice", map[string]int{"titled": 2, "watched": 1})
}

// Agent-deck's own preview pipe counts as a client: a lane just browsed in the TUI is being looked at.
func TestOpenCodePaneWatched_CountsEveryAttachedClient(t *testing.T) {
	skipIfNoTmuxBinary(t)
	socket := fmt.Sprintf("adeck-route-watch-%d", os.Getpid())
	t.Cleanup(func() { _ = tmux.Exec(socket, "kill-server").Run() })
	if out, err := tmux.Exec(socket, "new-session", "-d", "-s", "lane", "sleep", "600").CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	inst := &Instance{TmuxSocketName: socket, tmuxSession: &tmux.Session{Name: "lane", SocketName: socket}}
	if openCodePaneWatched(inst) {
		t.Fatal("lane with no client counted as watched")
	}

	client := tmux.Exec(socket, "-C", "attach-session", "-t", "lane")
	stdin, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = client.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for !openCodePaneWatched(inst) {
		if time.Now().After(deadline) {
			t.Fatal("lane with a control client attached counted as unwatched")
		}
		time.Sleep(50 * time.Millisecond)
	}

	gone := &Instance{TmuxSocketName: socket + "-gone", tmuxSession: &tmux.Session{Name: "lane", SocketName: socket + "-gone"}}
	if !openCodePaneWatched(gone) {
		t.Fatal("lane whose clients cannot be read counted as unwatched")
	}
}
