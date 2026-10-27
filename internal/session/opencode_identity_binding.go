package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/childenv"
)

const openCodeIdentityEntry = "agent-deck-identity"

// BindOpenCodeSession runs in the pane after env sources, without service recovery or session replacement.
func BindOpenCodeSession(ctx context.Context, server, sessionID string, create bool) error {
	var svc openCodeService
	var err error
	if server == "" {
		svc, err = openCodeRegisteredService()
		if err != nil {
			return fmt.Errorf("resolving OpenCode binding service: %w", err)
		}
	} else {
		server, err = parseOpenCodeServiceURL(server)
		if err != nil {
			return err
		}
		svc = openCodeService{URL: server, Password: openCodeServicePassword(server)}
	}
	sessionID, err = normalizeToolSessionID(FieldOpenCodeSessionID, sessionID)
	if err != nil || sessionID == "" {
		return fmt.Errorf("invalid OpenCode binding session id")
	}
	variables := map[string]string{}
	for _, pair := range childenv.ForSelfReexec("OPENCODE_PASSWORD", "OPENCODE_SERVER_PASSWORD") {
		name, value, _ := strings.Cut(pair, "=")
		variables[name] = value
	}
	if variables["AGENTDECK_INSTANCE_ID"] == "" {
		return fmt.Errorf("OpenCode binding requires AGENTDECK_INSTANCE_ID")
	}
	variables["OPENCODE_SESSION_ID"] = sessionID
	if raw, ok := variables["OPENCODE_CONFIG_CONTENT"]; ok {
		variables["OPENCODE_CONFIG_CONTENT"] = openCodeConfigWithoutIdentity(raw)
	}
	var identity string
	if file := variables[IdentityFileEnv]; file != "" {
		text, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("reading OpenCode session identity: %w", err)
		}
		identity = "This is the authoritative agent-deck identity for this OpenCode session. " +
			"It supersedes any agent-deck identity inherited from shared-service configuration. " +
			"Other agent-deck identity blocks describe other sessions, not this session.\n\n" + string(text)
	}
	path := "/api/session/" + sessionID
	status, out, err := openCodeBindingRequest(ctx, svc, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound && create {
		directory, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolving OpenCode session directory: %w", err)
		}
		status, out, err = openCodeBindingRequest(ctx, svc, http.MethodPost, "/api/session", map[string]any{
			"id": sessionID, "title": variables["AGENTDECK_TITLE"], "location": map[string]string{"directory": directory},
		})
		if err != nil {
			return err
		}
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("resolving OpenCode binding session: HTTP %d", status)
	}
	var reply struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &reply); err != nil || reply.Data.ID != sessionID {
		return fmt.Errorf("OpenCode binding response does not match session id")
	}
	entryPath := "/api/experimental/session/" + sessionID + "/instructions/entries/" + openCodeIdentityEntry
	method := http.MethodPut
	var body any = map[string]string{"value": identity}
	if identity == "" {
		method, body = http.MethodDelete, nil
	}
	if status, _, err = openCodeBindingRequest(ctx, svc, method, entryPath, body); err != nil {
		return err
	} else if status < 200 || status > 299 {
		return fmt.Errorf("binding OpenCode session instructions: HTTP %d", status)
	}
	status, _, err = openCodeBindingRequest(ctx, svc, http.MethodPut, path+"/environment", map[string]any{"variables": variables})
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("binding OpenCode session environment: HTTP %d", status)
	}
	return nil
}

func openCodeBindingRequest(ctx context.Context, svc openCodeService, method, path string, body any) (int, []byte, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding OpenCode binding request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, svc.URL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("creating OpenCode binding request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if svc.Password != "" {
		req.SetBasicAuth("opencode", svc.Password)
	}
	resp, err := openCodeSessionHTTPClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("sending OpenCode binding request: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, openCodeSessionResponseLimit))
	if err != nil {
		return 0, nil, fmt.Errorf("reading OpenCode binding response: %w", err)
	}
	return resp.StatusCode, out, nil
}

func openCodeConfigWithoutIdentity(raw string) string {
	var config map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &config) != nil {
		return raw
	}
	var instructions []string
	if json.Unmarshal(config["instructions"], &instructions) != nil {
		return raw
	}
	kept := make([]string, 0, len(instructions))
	for _, file := range instructions {
		managed := filepath.ToSlash(filepath.Clean(file))
		if filepath.Base(managed) == identityFileName && (strings.Contains(managed, "/agent-deck/runtime/identity/") || strings.Contains(managed, "/agent-deck/identity/")) {
			continue
		}
		kept = append(kept, file)
	}
	if len(kept) == len(instructions) {
		return raw
	}
	config["instructions"], _ = json.Marshal(kept)
	out, _ := json.Marshal(config)
	return string(out)
}

func openCodeServiceLaunchEnv() []string {
	var environment []string
	for _, pair := range childenv.ForLaunch("") {
		name, value, _ := strings.Cut(pair, "=")
		if strings.HasPrefix(name, "AGENTDECK_") || name == "TMUX" || name == "TMUX_PANE" || name == "OPENCODE_PASSWORD" || name == "OPENCODE_SERVER_PASSWORD" {
			continue
		}
		if name == "OPENCODE_CONFIG_CONTENT" {
			value = openCodeConfigWithoutIdentity(value)
		}
		environment = append(environment, name+"="+value)
	}
	return environment
}
