package session

import (
	"encoding/json"
	"time"
)

// clearOpenCodeSessionBinding drops the binding and marks the clear as
// intentional, so the next save writes it instead of carrying the old id
// forward: opencode_session_id is a sticky tool_data key.
func (i *Instance) clearOpenCodeSessionBinding() {
	i.OpenCodeSessionID = ""
	i.OpenCodeDetectedAt = time.Time{}
	i.OpenCodeSessionDir = ""
	i.openCodeSessionDirID = ""
	i.openCodeSessionIDCleared = true
}

// writeOpenCodeSessionClearToToolData states an empty binding explicitly when
// the clear was intentional, and otherwise leaves the id keys out so a writer
// that never saw the binding cannot wipe it.
func writeOpenCodeSessionClearToToolData(td json.RawMessage, intentional bool) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(td) > 0 {
		_ = json.Unmarshal(td, &m)
	}
	if intentional {
		m["opencode_session_id"] = json.RawMessage(`""`)
		m["opencode_detected_at"] = json.RawMessage(`0`)
	} else {
		delete(m, "opencode_session_id")
		delete(m, "opencode_detected_at")
	}
	out, _ := json.Marshal(m)
	return out
}
