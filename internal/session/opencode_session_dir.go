package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

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
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, ok := i.runOpenCodeCLI(ctx, projectPath, binary,
		"api", "session.get", "--param", "sessionID="+currentID)
	if !ok {
		return ""
	}
	var got openCodeServiceSessionGet
	if err := json.Unmarshal(output, &got); err != nil || got.Data.ID != currentID {
		return ""
	}
	sessionLog.Debug("opencode_bound_session_found",
		slog.String("session_id", currentID),
		slog.String("dir", logging.SanitizeValue(got.Data.flatten().Directory)))
	return currentID
}
