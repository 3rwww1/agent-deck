package session

import (
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
)

// The restart sweep kills every pane sharing OPENCODE_SESSION_ID, so a duplicate kills its sibling.

var readOpenCodeProcessTable = func() map[int]procowner.ProcCommand {
	if reader, ok := procowner.NewProber().(procowner.CommandReader); ok {
		if table, err := reader.Commands(); err == nil {
			return table
		}
	}
	return nil
}

var readOpenCodePanePID = func(inst *Instance) int {
	if tmuxSession := inst.GetTmuxSession(); tmuxSession != nil {
		if pid, err := tmuxSession.PanePID(); err == nil {
			return pid
		}
	}
	return 0
}

// UpdateOpenCodeSessionsWithDedup keeps each OpenCode session id on one lane
// and clears it, tmux env included, on every other lane bound to it.
func UpdateOpenCodeSessionsWithDedup(instances []*Instance) {
	lanesByID := map[string][]*Instance{}
	for _, inst := range instances {
		if inst != nil && inst.Tool == "opencode" && inst.OpenCodeSessionID != "" {
			lanesByID[inst.OpenCodeSessionID] = append(lanesByID[inst.OpenCodeSessionID], inst)
		}
	}
	var table map[int]procowner.ProcCommand
	for id, lanes := range lanesByID {
		if len(lanes) < 2 {
			continue
		}
		if table == nil {
			table = readOpenCodeProcessTable()
		}
		keeper := openCodeSessionKeeper(lanes, id, table)
		for _, inst := range lanes {
			if inst == keeper {
				continue
			}
			inst.clearOpenCodeSessionBinding()
			sessionLog.Warn("opencode_session_duplicate_cleared",
				slog.String("instance_id", inst.ID),
				slog.String("kept_by", keeper.ID),
				slog.String("session_id", id))
			if unset := makeGenericSessionEnvPostCommit(inst, "", []string{"OPENCODE_SESSION_ID"}); unset != nil {
				unset()
			}
		}
	}
}

// openCodeSessionKeeper picks the lane whose pane runs the session, else the
// first to bind it. The tmux env is no evidence: agent-deck writes it at bind.
func openCodeSessionKeeper(lanes []*Instance, id string, table map[int]procowner.ProcCommand) *Instance {
	sort.SliceStable(lanes, func(a, b int) bool {
		return openCodeBoundAt(lanes[a]).Before(openCodeBoundAt(lanes[b]))
	})
	if table != nil {
		for _, inst := range lanes {
			if paneRunsOpenCodeSession(table, readOpenCodePanePID(inst), id) {
				return inst
			}
		}
	}
	return lanes[0]
}

// openCodeBoundAt is when the lane took its binding. Rows written before the
// detection stamp existed fall back to when the lane was created.
func openCodeBoundAt(inst *Instance) time.Time {
	if !inst.OpenCodeDetectedAt.IsZero() {
		return inst.OpenCodeDetectedAt
	}
	return inst.CreatedAt
}

func paneRunsOpenCodeSession(table map[int]procowner.ProcCommand, panePID int, id string) bool {
	if panePID <= 0 {
		return false
	}
	children := map[int][]int{}
	for pid, row := range table {
		children[row.PPID] = append(children[row.PPID], pid)
	}
	seen := map[int]bool{}
	for queue := []int{panePID}; len(queue) > 0; queue = queue[1:] {
		pid := queue[0]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if openCodeArgvSessionID(table[pid].Args) == id {
			return true
		}
		queue = append(queue, children[pid]...)
	}
	return false
}

// openCodeArgvSessionID reads the session an `opencode` command line resumes,
// whether the TUI owns its service or attaches with --server.
func openCodeArgvSessionID(args []string) string {
	for idx, arg := range args {
		if filepath.Base(arg) != "opencode" {
			continue
		}
		for j := idx + 1; j < len(args); j++ {
			if (args[j] == "-s" || args[j] == "--session") && j+1 < len(args) {
				return args[j+1]
			}
			if id, ok := strings.CutPrefix(args[j], "--session="); ok {
				return id
			}
		}
	}
	return ""
}
