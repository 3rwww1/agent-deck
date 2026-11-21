package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeSessionName(t *testing.T) {
	for _, tc := range []struct {
		name, body, group, want string
	}{
		{"off by default", "", "team", "lane"},
		{"on", "native_name_from_path=true", "team", "team_lane"},
		{"nested group", "native_name_from_path=true", "team/sub", "team_sub_lane"},
		{"no group", "native_name_from_path=true", "", "lane"},
		{"off", "native_name_from_path=false", "team", "lane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startupNameConfig(t, tc.body)
			inst := &Instance{Tool: "claude", Title: "lane", GroupPath: tc.group}
			if got := inst.NativeSessionName(); got != tc.want {
				t.Errorf("NativeSessionName() = %q, want %q", got, tc.want)
			}
			if got := inst.ClaudeLaunchName(); got != tc.want {
				t.Errorf("ClaudeLaunchName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNativeSessionNameKeepsPushTitleOptOut(t *testing.T) {
	startupNameConfig(t, "native_name_from_path=true\npush_title=false")
	if got := (&Instance{Tool: "claude", Title: "lane", GroupPath: "team"}).ClaudeLaunchName(); got != "" {
		t.Errorf("ClaudeLaunchName() = %q with push_title=false, want none", got)
	}
}

// A name stored before the option existed must not pin the lane to it.
func TestNativeSessionNameReplacesStoredName(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		extra      []string
		want       []string
	}{
		{"long flag", "native_name_from_path=true", []string{"--model", "opus", "--name", "team/lane"}, []string{"--name", "team_lane", "--model", "opus"}},
		{"short flag", "native_name_from_path=true", []string{"-n", "old"}, []string{"--name", "team_lane"}},
		{"equals form", "native_name_from_path=true", []string{"--name=old"}, []string{"--name", "team_lane"}},
		// A token merely starting with -n is not stripped, so it keeps the name.
		{"attached short", "native_name_from_path=true", []string{"-nold"}, []string{"-nold"}},
		{"option off", "", []string{"--name", "mine"}, []string{"--name", "mine"}},
		{"push_title off", "native_name_from_path=true\npush_title=false", []string{"--name", "mine"}, []string{"--name", "mine"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startupNameConfig(t, tc.body)
			inst := &Instance{Tool: "claude", Title: "lane", GroupPath: "team", ExtraArgs: tc.extra, IdentityInjectionDisabled: true}
			got := startupArgs(t, inst.buildClaudeExtraFlags(nil))
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("argv = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNativeSessionNameReplacesStoredNameOnFork(t *testing.T) {
	for _, tc := range []struct{ name, title, want string }{
		{"target named", "child", "--name team_child"},
		// No path name for the target: the stored name must survive.
		{"target unnamed", "a\nb", "--name old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startupNameConfig(t, "native_name_from_path=true")
			parent := &Instance{Tool: "claude", Title: "lane", GroupPath: "team", ExtraArgs: []string{"--name", "old"}, ClaudeSessionID: "11111111-1111-4111-8111-111111111111", ClaudeDetectedAt: time.Now()}
			parent.markClaudeSessionIDVerified()
			target := &Instance{Tool: "claude", Title: tc.title, GroupPath: "team", ProjectPath: t.TempDir(), ExtraArgs: append([]string(nil), parent.ExtraArgs...)}
			cmd, err := parent.buildClaudeForkCommandForTarget(target, nil)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(cmd, "--name") != 1 || !strings.Contains(cmd, tc.want) {
				t.Fatalf("fork command = %s, want one %q", cmd, tc.want)
			}
		})
	}
}

// The name agent-deck pushed must not come back as the deck title.
func TestNativeSessionNameIsNotSyncedBack(t *testing.T) {
	startupNameConfig(t, "native_name_from_path=true")
	seedClaudeSession(t, os.Getenv("HOME"), "sid-path", "team_lane")
	inst := &Instance{ID: "i1", Tool: "claude", Title: "lane", GroupPath: "team"}
	if name, changed := inst.ResolveTitleFromClaude("sid-path"); changed {
		t.Fatalf("ResolveTitleFromClaude = (%q,true), want no change", name)
	}
	seedClaudeSession(t, os.Getenv("HOME"), "sid-renamed", "renamed")
	if name, changed := inst.ResolveTitleFromClaude("sid-renamed"); !changed || name != "renamed" {
		t.Fatalf("ResolveTitleFromClaude = (%q,%v), want (renamed,true)", name, changed)
	}
}

func TestOpenCodeNativeNamePushedWhenSessionAppears(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	var mu sync.Mutex
	var patches []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/info":
			fmt.Fprint(w, `{"version":"2.0.25"}`)
		case r.URL.Path == "/api/session/ses_LANE" && r.Method == http.MethodPatch:
			mu.Lock()
			defer mu.Unlock()
			calls++
			// The TUI creates the minted session a moment after launch.
			if calls == 1 {
				http.NotFound(w, r)
				return
			}
			body, _ := io.ReadAll(r.Body)
			patches = append(patches, string(body))
			fmt.Fprint(w, `{"data":{"id":"ses_LANE"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	useOpenCodeStub(t, fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = 'service status' ] && echo %s\n", srv.URL))
	enableNativeNameFromPath(t)
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	if err := os.MkdirAll(stateHome+"/opencode", 0o700); err != nil {
		t.Fatal(err)
	}
	reg := fmt.Sprintf(`{"url":%q,"pid":%d,"password":""}`, srv.URL, os.Getpid())
	if err := os.WriteFile(stateHome+"/opencode/service.json", []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	useFreshOpenCodeServiceGuard(t)

	inst := &Instance{ID: "oc", Tool: "opencode", Title: "lane", GroupPath: "team", OpenCodeSessionID: "ses_LANE"}
	inst.pushOpenCodeNativeName(5*time.Second, 10*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(patches) != 1 {
		t.Fatalf("patches = %q, want one", patches)
	}
	var got struct{ Title string }
	if err := json.Unmarshal([]byte(patches[0]), &got); err != nil || got.Title != "team_lane" {
		t.Fatalf("patch body = %s, want title team_lane", patches[0])
	}
}

func TestOpenCodeNativeNameOffSendsNothing(t *testing.T) {
	startupNameConfig(t, "")
	useOpenCodeStub(t, "#!/bin/sh\necho called >&2\nexit 1\n")
	inst := &Instance{ID: "oc", Tool: "opencode", Title: "lane", GroupPath: "team", OpenCodeSessionID: "ses_LANE"}
	start := time.Now()
	inst.pushOpenCodeNativeName(time.Second, 10*time.Millisecond)
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("push with the option off took %s, want an immediate return", time.Since(start))
	}
}

// enableNativeNameFromPath turns the option on in the config useOpenCodeStub wrote.
func enableNativeNameFromPath(t *testing.T) {
	t.Helper()
	path, err := GetUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("native_name_from_path = true\n"), body...), 0o600); err != nil {
		t.Fatal(err)
	}
	ClearUserConfigCache()
}
