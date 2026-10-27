package session

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// An OpenCode upgrade replaces the installed CLI while the service it spawned keeps
// running the old binary, which the package manager may already have deleted. The old
// server still answers, so a stall is never the evidence (#55): only a probe that
// answers with another version than the installed CLI restarts the service, once per
// version pair, under the lock that already serializes the stopped-service start.

// openCodeSkewRestartName marks the (old, new) version pair one restart was spent on.
const openCodeSkewRestartName = "opencode-service-skew"

// reconcileOpenCodeSkew restarts the service once when it answers on another version
// than the installed CLI, on the installed version. It runs under the service lock, so
// the marker it writes in the locks dir holds for every agent-deck process.
func (g *openCodeServiceGuard) reconcileOpenCodeSkew(locksDir, binary string, out *checkOutcome, serverVersion string) {
	if serverVersion == "" {
		return
	}
	installedVersion, ok := probeOpenCodeCLIVersion(binary)
	if !ok || installedVersion == serverVersion {
		return
	}
	pair := serverVersion + " " + installedVersion
	marker := filepath.Join(locksDir, openCodeSkewRestartName)
	if tried, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(tried)) == pair {
		// The one restart this pair was allowed is already spent, here or in another process.
		sessionLog.Debug("opencode_service_version_skew",
			slog.String("old_version", serverVersion), slog.String("new_version", installedVersion))
		return
	}
	sessionLog.Warn("opencode_service_version_skew",
		slog.String("old_version", serverVersion), slog.String("new_version", installedVersion))
	// The marker goes down before the attempt, so a restart that fails is still spent.
	if err := os.WriteFile(marker, []byte(pair+"\n"), 0o600); err != nil {
		sessionLog.Warn("opencode_service_skew_restart_failed",
			slog.String("old_version", serverVersion), slog.String("new_version", installedVersion),
			slog.String("error", err.Error()))
		return
	}
	sessionLog.Warn("opencode_service_skew_restart",
		slog.String("old_version", serverVersion), slog.String("new_version", installedVersion))
	if err := g.run(binary, "restart"); err != nil {
		sessionLog.Warn("opencode_service_skew_restart_failed",
			slog.String("old_version", serverVersion), slog.String("new_version", installedVersion),
			slog.String("error", err.Error()))
		return
	}
	out.started = true
	if out.svc, out.err = g.status(binary); out.err == nil {
		_, out.err = g.probe(out.svc)
	}
}
