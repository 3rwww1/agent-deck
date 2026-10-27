package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/childenv"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// TestMain also changes HOME, so capture the Go caches before either isolation layer runs.
var openCodeIdentityBuildCaches, openCodeIdentityBuildCachesErr = func() ([]byte, error) {
	cmd := exec.Command("go", "env", "GOMODCACHE", "GOCACHE")
	cmd.Env = childenv.ForLaunch("")
	return cmd.Output()
}()

func TestOpenCodeIdentitySharedServiceLaunchBoundary(t *testing.T) {
	identityTestEnv(t)
	pinOpenCodeMajorVersion(t, 2, true)
	useOpenCodeStub(t, "#!/bin/sh\nexit 0\n")
	g := useFreshOpenCodeServiceGuard(t)
	g.mu.Lock()
	g.markHealthy(openCodeService{URL: "http://127.0.0.1:4096"})
	g.mu.Unlock()
	for _, tc := range []struct {
		name  string
		bound bool
		opts  *OpenCodeOptions
	}{
		{"fresh", false, nil},
		{"resume", true, nil},
		{"resume with model", true, &OpenCodeOptions{Model: "user/model"}},
		{"fresh with model", false, &OpenCodeOptions{Model: "user/model"}},
		{"continue", false, &OpenCodeOptions{SessionMode: "continue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := identityTestInstance("opencode")
			inst.Command = "opencode"
			if tc.bound {
				inst.OpenCodeSessionID = "ses_BOUND"
			}
			if tc.opts != nil {
				_ = inst.SetOpenCodeOptions(tc.opts)
			}
			command := inst.buildOpenCodeCommand("opencode")
			if strings.Contains(command, `export OPENCODE_CONFIG_CONTENT=`) {
				t.Error("identity still enters shared service config")
			}
			if !strings.Contains(command, "session bind-opencode") || !strings.Contains(command, "--session-id ") {
				t.Error("attached launch never binds its selected session environment/instructions")
			}
		})
	}
}

func TestOpenCodeIdentitySharedServiceTwoSessionsAndResume(t *testing.T) {
	testOpenCodeIdentitySharedServiceTwoSessionsAndResume(t, false)
}

func TestOpenCodeIdentitySharedServiceForkFirstStart(t *testing.T) {
	testOpenCodeIdentitySharedServiceTwoSessionsAndResume(t, true)
}

func testOpenCodeIdentitySharedServiceTwoSessionsAndResume(t *testing.T, fork bool) {
	t.Helper()
	if openCodeIdentityBuildCachesErr != nil {
		t.Fatalf("resolve fixture Go caches: %v", openCodeIdentityBuildCachesErr)
	}
	caches := strings.Split(strings.TrimSpace(string(openCodeIdentityBuildCaches)), "\n")
	if len(caches) != 2 || caches[0] == "" || caches[1] == "" {
		t.Fatal("invalid fixture Go caches")
	}
	identityTestEnv(t)
	pinOpenCodeMajorVersion(t, 2, true)
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "agent-deck"), "../../cmd/agent-deck")
	build.Env = append(childenv.ForLaunch(""), "GOMODCACHE="+caches[0], "GOCACHE="+caches[1])
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture binary: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "")
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"model":"user/model","instructions":["/user/rules.md"]}`)
	t.Setenv("UNRELATED_SETTING", "preserved")
	t.Setenv("OPENCODE_PASSWORD", "excluded-fixture-password")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "excluded-legacy-fixture-password")
	storage, err := NewStorageWithProfile(sessionProfileEnvValue())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	instances := []*Instance{
		{ID: "lane_A", Title: "first", Tool: "opencode", Command: "opencode", ProjectPath: t.TempDir(), OpenCodeSessionID: "ses_A", ParentSessionID: "community", GroupPath: "workers"},
		{ID: "lane_B", Title: "second", Tool: "opencode", Command: "opencode", ProjectPath: t.TempDir(), OpenCodeSessionID: "ses_B", GroupPath: "supervisor"},
	}
	paneEnvironments := map[string][]string{}
	for _, inst := range instances {
		inst.tmuxSession = tmux.NewSession(inst.Title, inst.ProjectPath)
		name := inst.tmuxSession.TmuxName()
		if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep", "300").CombinedOutput(); err != nil {
			t.Fatalf("create isolated pane: %v\n%s", err, out)
		}
		t.Cleanup(func() { _ = inst.tmuxSession.Kill() })
		out, err := exec.Command("tmux", "display-message", "-p", "-t", name, "#{socket_path},#{pid},#{session_id}\n#{pane_id}").Output()
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(fields) != 2 {
			t.Fatal("invalid isolated pane environment")
		}
		paneEnvironments[inst.ID] = []string{"TMUX=" + fields[0], "TMUX_PANE=" + fields[1]}
	}
	if err := storage.SaveWithGroups(instances, NewGroupTreeWithGroups(instances, nil)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	environments := map[string]map[string]string{}
	environmentWrites := map[string]int{}
	instructionWrites := map[string]int{}
	entries := map[string]map[string]string{
		"ses_A": {"user-guidance": "keep first guidance", "agent-deck-identity": "stale first identity"},
		"ses_B": {"user-guidance": "keep second guidance", "agent-deck-identity": "stale second identity"},
	}
	unchanged := map[string]string{"transcript": "all previous turns", "task": "active task", "composer": "unsent draft", "model": "provider/existing", "parentID": "ses_parent"}
	metadata := map[string]map[string]string{}
	for _, inst := range instances {
		metadata[inst.OpenCodeSessionID] = map[string]string{"id": inst.OpenCodeSessionID}
		for key, value := range unchanged {
			metadata[inst.OpenCodeSessionID][key] = value
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if fork && r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_A/fork" {
			entries["ses_B"] = map[string]string{}
			for key, value := range entries["ses_A"] {
				entries["ses_B"][key] = value
			}
			metadata["ses_B"]["parentID"] = "ses_A"
			_ = json.NewEncoder(w).Encode(map[string]any{"data": metadata["ses_B"]})
			return
		}
		if fork && r.Method == http.MethodPatch && r.URL.Path == "/api/session/ses_B" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if fork && r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_B/move" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "session" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": metadata[parts[2]]})
			return
		}
		if r.Method == http.MethodPut && len(parts) == 4 && parts[3] == "environment" {
			var payload struct {
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid environment", http.StatusBadRequest)
				return
			}
			environments[parts[2]] = payload.Variables
			environmentWrites[parts[2]]++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPut && len(parts) == 7 && parts[4] == "instructions" && parts[6] == "agent-deck-identity" {
			var payload struct {
				Value string `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			entries[parts[3]][parts[6]] = payload.Value
			instructionWrites[parts[3]]++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "unexpected mutation", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	savedPath := os.Getenv("PATH")
	useOpenCodeStub(t, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+savedPath)
	g := useFreshOpenCodeServiceGuard(t)
	g.mu.Lock()
	g.markHealthy(openCodeService{URL: srv.URL})
	g.mu.Unlock()
	for round := 0; round < 2; round++ {
		for index, inst := range instances {
			if fork && round == 0 && index == 1 {
				instances[0].OpenCodeDetectedAt = time.Now()
				child, _, err := instances[0].CreateForkedOpenCodeInstanceWithOptionsAndWorkDir(inst.Title, inst.GroupPath, &OpenCodeOptions{}, inst.ProjectPath, "", "")
				if err != nil {
					t.Fatal(err)
				}
				child.ParentSessionID = instances[0].ID
				child.tmuxSession = inst.tmuxSession
				paneEnvironments[child.ID] = paneEnvironments[inst.ID]
				instances[index] = child
				inst = child
				if err := storage.SaveWithGroups(instances, NewGroupTreeWithGroups(instances, nil)); err != nil {
					t.Fatal(err)
				}
			}
			command := inst.Command
			if inst.IsForkAwaitingStart {
				command = inst.consumeForkStartCommand()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			launch := exec.CommandContext(ctx, "sh", "-c", inst.buildOpenCodeCommand(command))
			launch.Dir = inst.ProjectPath
			launch.Env = append(childenv.ForSelfReexec(), paneEnvironments[inst.ID]...)
			out, err := launch.CombinedOutput()
			cancel()
			if err != nil {
				t.Fatalf("launch %s: %v\n%s", inst.ID, err, out)
			}
			if fork && round == 0 && index == 1 {
				mu.Lock()
				if environments[inst.OpenCodeSessionID]["AGENTDECK_INSTANCE_ID"] != inst.ID || !strings.Contains(entries[inst.OpenCodeSessionID]["agent-deck-identity"], inst.BuildIdentityPrompt()) {
					t.Error("fork first start retained parent identity instead of binding child")
				}
				mu.Unlock()
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for index, inst := range instances {
		if environmentWrites[inst.OpenCodeSessionID] != 2 || instructionWrites[inst.OpenCodeSessionID] != 2 {
			t.Errorf("%s: resume did not refresh its native binding", inst.ID)
		}
		vars := environments[inst.OpenCodeSessionID]
		path, _ := inst.IdentityFilePath()
		if vars["AGENTDECK_INSTANCE_ID"] != inst.ID || vars[IdentityFileEnv] != path || vars["OPENCODE_SESSION_ID"] != inst.OpenCodeSessionID || vars["UNRELATED_SETTING"] != "preserved" {
			t.Errorf("%s: session environment missing or inherited another lane", inst.ID)
		}
		if _, ok := vars["OPENCODE_PASSWORD"]; ok {
			t.Error("server password entered shell environment")
		}
		if _, ok := vars["OPENCODE_SERVER_PASSWORD"]; ok {
			t.Error("legacy password entered shell environment")
		}
		if vars["OPENCODE_CONFIG_CONTENT"] != os.Getenv("OPENCODE_CONFIG_CONTENT") {
			t.Error("unrelated user config changed")
		}
		instruction := entries[inst.OpenCodeSessionID]["agent-deck-identity"]
		other := instances[1-index]
		guidance := []string{"keep first guidance", "keep second guidance"}[index]
		if fork && index == 1 {
			guidance = "keep first guidance"
		}
		if !strings.Contains(instruction, inst.BuildIdentityPrompt()) || strings.Contains(instruction, "- session id: "+other.ID) || entries[inst.OpenCodeSessionID]["user-guidance"] != guidance {
			t.Errorf("%s: model instructions missing, leaked or replaced user guidance", inst.ID)
		}
		for key, value := range unchanged {
			if fork && index == 1 && key == "parentID" {
				value = "ses_A"
			}
			if metadata[inst.OpenCodeSessionID][key] != value {
				t.Errorf("binding changed %s", key)
			}
		}
		if len(vars) == 0 {
			continue
		}
		current := exec.Command(filepath.Join(bin, "agent-deck"), "session", "current", "--json")
		for name, value := range vars {
			current.Env = append(current.Env, name+"="+value)
		}
		out, err := current.Output()
		if err != nil {
			t.Fatalf("session current %s: %v\n%s", inst.ID, err, out)
		}
		var record struct {
			ID              string `json:"id"`
			Title           string `json:"title"`
			ParentSessionID string `json:"parent_session_id"`
			IdentityFile    string `json:"identity_file"`
			TmuxSession     string `json:"tmux_session"`
		}
		if err := json.Unmarshal(out, &record); err != nil {
			t.Fatal(err)
		}
		if record.ID != inst.ID || record.Title != inst.Title || record.ParentSessionID != inst.ParentSessionID || record.IdentityFile != path || record.TmuxSession != inst.tmuxSession.TmuxName() {
			t.Errorf("session current returned another session: %+v", record)
		}
	}
	loaded, _, err := storage.LoadWithGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, restored := range loaded {
		for _, inst := range instances {
			if restored.ID == inst.ID && (restored.ParentSessionID != inst.ParentSessionID || restored.Title != inst.Title || restored.OpenCodeSessionID != inst.OpenCodeSessionID) {
				t.Error("binding changed persisted session identity")
			}
		}
	}
}

func TestOpenCodeIdentityServiceStartDoesNotInheritLane(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "environment")
	binary := filepath.Join(dir, "opencode")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$AGENTDECK_INSTANCE_ID\" \"$AGENTDECK_IDENTITY_FILE\" \"$TMUX\" \"$OPENCODE_CONFIG_CONTENT\" \"$UNRELATED_SETTING\" > \"$RECORD\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECORD", record)
	t.Setenv("AGENTDECK_INSTANCE_ID", "lane_A")
	t.Setenv(IdentityFileEnv, "/home/user/.local/share/agent-deck/runtime/identity/lane_A/identity.md")
	t.Setenv("TMUX", "fixture-socket,1,0")
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"model":"user/model","instructions":["/user/rules.md","/home/user/.local/share/agent-deck/runtime/identity/lane_A/identity.md"]}`)
	t.Setenv("UNRELATED_SETTING", "kept")
	if err := runOpenCodeServiceAction(binary, "start"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("record has %d lines, want 5", len(lines))
	}
	for index := 0; index < 3; index++ {
		if lines[index] != "" {
			t.Errorf("lane-specific service environment field %d survived", index)
		}
	}
	var cfg struct {
		Model        string   `json:"model"`
		Instructions []string `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(lines[3]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "user/model" || len(cfg.Instructions) != 1 || cfg.Instructions[0] != "/user/rules.md" || lines[4] != "kept" {
		t.Fatal("service startup leaked lane instructions or changed unrelated config/env")
	}
}
