package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
)

// OpenCodeSSETarget returns the event stream that feeds this instance's
// status, or false when status stays on tmux content sniffing.
//
//   - 1.x launched with --port: the TUI's own server, as before (issue #1614).
//   - 2.x with a bound session id: the shared background service, filtered to
//     that session (issue #2511).
//
// The 2.x check is the memoised version probe the launch already used; the
// service itself is looked up later, inside the stream goroutine.
func (i *Instance) OpenCodeSSETarget() (SSETarget, bool) {
	if i.Tool != "opencode" {
		return SSETarget{}, false
	}
	if port := i.GetOpenCodePort(); port > 0 {
		return SSETarget{InstanceID: i.ID, Port: port}, true
	}
	i.mu.RLock()
	sessionID := i.OpenCodeSessionID
	i.mu.RUnlock()
	if sessionID == "" {
		return SSETarget{}, false
	}
	if config, err := LoadUserConfig(); err == nil && config != nil && config.OpenCode.DisableSSEStatus {
		return SSETarget{}, false
	}
	if !i.openCodeRejectsV1LaunchFlags() {
		return SSETarget{}, false
	}
	return SSETarget{InstanceID: i.ID, sessionID: sessionID, service: i.resolveOpenCodeService}, true
}

// resolveOpenCodeService reuses the guard's checked service so reconnects skip `service status`.
func (i *Instance) resolveOpenCodeService() (openCodeService, error) {
	binary, ok := i.openCodeLaunchBinary()
	if !ok {
		return openCodeService{}, errors.New("opencode binary not found")
	}
	if svc, ok := sharedOpenCodeService.attach(binary); ok {
		return svc, nil
	}
	return openCodeServiceStatus(binary)
}

// openCodeServiceStatus trusts the registration file's password only for the URL the binary reports.
func openCodeServiceStatus(binary string) (openCodeService, error) {
	out, err := runModelProbeCommand(binary, "service", "status")
	if err != nil {
		return openCodeService{}, err
	}
	if strings.TrimSpace(string(out)) == "stopped" {
		return openCodeStoppedServiceStatus(binary)
	}
	serviceURL, err := parseOpenCodeServiceURL(string(out))
	if err != nil {
		return openCodeService{}, err
	}
	return openCodeService{URL: serviceURL, Password: openCodeServicePassword(serviceURL)}, nil
}

// parseOpenCodeServiceURL takes the first line of `opencode service status`
// and accepts it only as an http(s) URL on this machine.
func parseOpenCodeServiceURL(out string) (string, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	line = strings.TrimRight(strings.TrimSpace(line), "/")
	u, err := url.Parse(line)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("opencode service status: not a service URL: %q", line)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("opencode service status: refusing non-loopback service %q", host)
	}
	return line, nil
}

var (
	errOpenCodeServiceStopped  = errors.New("opencode background service is stopped")
	errOpenCodeServiceDisabled = errors.New("opencode background service is disabled")
)

// openCodeStoppedServiceStatus splits "stopped" into disabled, stalled (registered, alive) and stopped.
func openCodeStoppedServiceStatus(binary string) (openCodeService, error) {
	if out, err := runModelProbeCommand(binary, "service", "get", "disabled"); err == nil && strings.TrimSpace(string(out)) == "true" {
		return openCodeService{}, errOpenCodeServiceDisabled
	}
	reg, ok := readOpenCodeServiceRegistration()
	if !ok || !procowner.Alive(reg.PID) {
		return openCodeService{}, errOpenCodeServiceStopped
	}
	serviceURL, err := parseOpenCodeServiceURL(reg.URL)
	if err != nil {
		return openCodeService{}, err
	}
	return openCodeService{URL: serviceURL, Password: reg.Password}, nil
}

type openCodeServiceRegistration struct {
	URL      string `json:"url"`
	PID      int    `json:"pid"`
	Password string `json:"password"`
}

// readOpenCodeServiceRegistration reads the file OpenCode 2.x writes for its running service.
func readOpenCodeServiceRegistration() (openCodeServiceRegistration, bool) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return openCodeServiceRegistration{}, false
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	data, err := os.ReadFile(filepath.Join(stateHome, "opencode", "service.json"))
	if err != nil {
		return openCodeServiceRegistration{}, false
	}
	var reg openCodeServiceRegistration
	if json.Unmarshal(data, &reg) != nil {
		return openCodeServiceRegistration{}, false
	}
	reg.URL = strings.TrimRight(reg.URL, "/")
	return reg, true
}

func openCodeServicePassword(serviceURL string) string {
	if reg, ok := readOpenCodeServiceRegistration(); ok && reg.URL == serviceURL {
		return reg.Password
	}
	return ""
}
