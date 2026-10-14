package session

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Each `opencode api` call starts one OpenCode process, so with a healthy service agent-deck sends the request itself.
func TestOpenCodeServiceCalls_GoStraightToHealthyService(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	const password = "registration-secret"
	projectPath := t.TempDir()
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "opencode" || pass != password {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/info" {
			fmt.Fprint(w, `{"version":"2.0.25"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) > 0 && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "json only", http.StatusUnsupportedMediaType)
			return
		}
		request := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			request += "?" + r.URL.Query().Encode()
		}
		if len(body) > 0 {
			request += " " + string(body)
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/session/ses_MINE":
			fmt.Fprint(w, movedSessionPayload("ses_MINE", projectPath))
		case "/api/session/ses_GONE":
			http.NotFound(w, r)
		default:
			fmt.Fprint(w, `{"id":"ses_CHILD"}`)
		}
	}))
	t.Cleanup(srv.Close)
	spawns := filepath.Join(t.TempDir(), "spawns")
	useOpenCodeStub(t, fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = 'service status' ] && { echo %s; exit 0; }\necho \"$*\" >> %s\n", srv.URL, spawns))
	registerOpenCodeService(t, srv.URL, os.Getpid(), password)
	useFreshOpenCodeServiceGuard(t)

	inst := &Instance{Tool: "opencode", ProjectPath: projectPath, OpenCodeSessionID: "ses_MINE"}
	binary, ok := inst.openCodeLaunchBinary()
	if !ok {
		t.Fatal("no opencode binary")
	}
	if out, err := runOpenCodeServiceCall(binary, projectPath, "session.fork", "ses_PARENT", "{}"); err != nil || strings.TrimSpace(string(out)) != `{"id":"ses_CHILD"}` {
		t.Fatalf("session.fork = %q, %v", out, err)
	}
	if _, err := runOpenCodeServiceCall(binary, projectPath, "session.get", "ses_GONE", ""); err == nil || err.Error() != "HTTP 404 Not Found" {
		t.Fatalf("session.get of a missing session: err = %v, want the CLI's text %q", err, "HTTP 404 Not Found")
	}
	if got := inst.queryOpenCodeSession(); got != "ses_MINE" {
		t.Fatalf("session ID = %q, want ses_MINE", got)
	}

	if spawned, _ := os.ReadFile(spawns); len(spawned) > 0 {
		t.Fatalf("spawned opencode for calls a healthy service could serve:\n%s", spawned)
	}
	want := []string{"POST /api/session/ses_PARENT/fork {}", "GET /api/session/ses_GONE", "GET /api/session/ses_MINE"}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("service requests =\n%s\nwant\n%s", strings.Join(requests, "\n"), strings.Join(want, "\n"))
	}
}
