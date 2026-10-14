package session

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/asheshgoplani/agent-deck/internal/logging"
)

// OpenCode 2.x can move a session to another directory (the session_move
// tool, or the TUI's move command). The id still resumes it, but a listing
// filtered by ProjectPath stops seeing it, so a bound instance reads its
// session by id from the service instead of paging the directory listing.

type openCodeServiceSessionGet struct {
	Data openCodeHTTPSessionMetadata `json:"data"`
}

// queryBoundOpenCodeServiceSession reads the bound session by id from the 2.x
// shared service. It returns currentID while the session exists, and "" (keep
// the binding as is) on any failure.
func (i *Instance) queryBoundOpenCodeServiceSession(projectPath, currentID string) string {
	binary, ok := i.openCodeLaunchBinary()
	if !ok {
		logBoundOpenCodeLookupFailed(currentID, "no opencode binary")
		return ""
	}
	output, err := runOpenCodeServiceCall(binary, projectPath, "session.get", currentID, "")
	if err != nil {
		logBoundOpenCodeLookupFailed(currentID, err.Error())
		return ""
	}
	var got openCodeServiceSessionGet
	if err := json.Unmarshal(output, &got); err != nil {
		logBoundOpenCodeLookupFailed(currentID, err.Error())
		return ""
	}
	if got.Data.ID != currentID {
		logBoundOpenCodeLookupFailed(currentID, fmt.Sprintf("reply carries session %q", got.Data.ID))
		return ""
	}
	sessionLog.Debug("opencode_bound_session_found",
		slog.String("session_id", currentID),
		slog.String("dir", logging.SanitizeValue(got.Data.flatten().Directory)))
	return currentID
}

func logBoundOpenCodeLookupFailed(sessionID, cause string) {
	sessionLog.Debug("opencode_bound_session_lookup_failed",
		slog.String("session_id", sessionID),
		slog.String("error", logging.SanitizeValue(cause)))
}
