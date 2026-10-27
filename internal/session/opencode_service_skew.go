package session

import (
	"log/slog"
	"os"
	"path/filepath"
)

// An OpenCode upgrade replaces the installed CLI while the service it spawned keeps
// running the old binary, which the package manager may already have deleted. The old
// server still answers, so a stall is never the evidence (#55): only a probe that
// answers on an older version than the installed CLI restarts the service, once per
// version pair, under the lock that already serializes the stopped-service start.

// openCodeSkewRestartName prefixes the marker files, one per (old, new) version
// pair, that record the pairs one restart was spent on.
const openCodeSkewRestartName = "opencode-service-skew"

// openCodeSkewMarker names the marker of one (old, new) version pair.
func openCodeSkewMarker(locksDir, oldVersion, newVersion string) string {
	return filepath.Join(locksDir, openCodeSkewRestartName+"-"+oldVersion+"-to-"+newVersion)
}

// reconcileOpenCodeSkew restarts the service once when it answers on an older
// version than the installed CLI, on the installed version. It runs under the
// service lock, so the markers it writes in the locks dir hold for every
// agent-deck process. A CLI older than the answering server never restarts: with
// the binary resolved per instance, one host can hold several CLI versions, and
// restarts toward each of them would loop forever.
func (g *openCodeServiceGuard) reconcileOpenCodeSkew(locksDir, binary string, out *checkOutcome, serverVersion string) {
	serverVersion = normalizeOpenCodeVersion(serverVersion)
	if serverVersion == "" {
		return
	}
	installedVersion, ok := probeOpenCodeCLIVersion(binary)
	if !ok {
		return
	}
	installedVersion = normalizeOpenCodeVersion(installedVersion)
	if installedVersion == serverVersion {
		return
	}
	if compareOpenCodeVersions(installedVersion, serverVersion) < 0 {
		sessionLog.Debug("opencode_service_version_skew_older_cli",
			slog.String("server_version", serverVersion), slog.String("cli_version", installedVersion))
		return
	}
	marker := openCodeSkewMarker(locksDir, serverVersion, installedVersion)
	if _, err := os.Stat(marker); err == nil {
		// The one restart this pair was allowed is already spent, here or in another process.
		sessionLog.Debug("opencode_service_version_skew",
			slog.String("old_version", serverVersion), slog.String("new_version", installedVersion))
		return
	}
	sessionLog.Warn("opencode_service_version_skew",
		slog.String("old_version", serverVersion), slog.String("new_version", installedVersion))
	// The marker goes down before the attempt, so a restart that fails or lands on
	// the old version again is still spent.
	if err := os.WriteFile(marker, []byte(serverVersion+" "+installedVersion+"\n"), 0o600); err != nil {
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
