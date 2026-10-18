package session

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"
)

// NativeSessionName is the name agent-deck gives the tool's own session: the
// deck title, after the group path when native_name_from_path is on.
func (i *Instance) NativeSessionName() string {
	i.mu.RLock()
	title, group := i.Title, i.GroupPath
	i.mu.RUnlock()
	if group == "" || !nativeNameFromPathEnabled() {
		return title
	}
	// Session names cannot hold "/", so path levels are joined with "_".
	return strings.ReplaceAll(group, "/", "_") + "_" + title
}

func nativeNameFromPathEnabled() bool {
	cfg, err := LoadUserConfig()
	return err == nil && cfg != nil && cfg.GetNativeNameFromPath()
}

// pushOpenCodeNativeName titles a 2.x lane's session once the TUI has created
// it, retrying every poll until wait runs out.
func (i *Instance) pushOpenCodeNativeName(wait, poll time.Duration) {
	if !nativeNameFromPathEnabled() {
		return
	}
	name := claudeTitleName(i.NativeSessionName())
	binary, ok := i.openCodeLaunchBinary()
	i.mu.RLock()
	sessionID, dir := i.OpenCodeSessionID, i.ProjectPath
	i.mu.RUnlock()
	if name == "" || !ok || sessionID == "" {
		return
	}
	body, err := json.Marshal(struct {
		Title string `json:"title"`
	}{name})
	if err != nil {
		return
	}
	deadline := time.Now().Add(wait)
	for {
		_, err := runOpenCodeServiceCall(binary, dir, "session.update", sessionID, string(body))
		if err == nil {
			sessionLog.Info("opencode_native_name_set",
				slog.String("instance_id", i.ID),
				slog.String("session_id", sessionID))
			return
		}
		if time.Now().After(deadline) {
			sessionLog.Warn("opencode_native_name_failed",
				slog.String("instance_id", i.ID),
				slog.String("session_id", sessionID),
				slog.String("error", err.Error()))
			return
		}
		time.Sleep(poll)
	}
}
