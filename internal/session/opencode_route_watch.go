package session

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// An attached 2.x TUI can come back from a service restart on its home screen,
// off the session the lane is bound to, and stays there.

const (
	openCodeHomeTitle = "OpenCode"
	// A lane still inside its launch wait has not shown its session yet.
	openCodeRouteSettle = openCodeServiceWaitLimit
)

// openCodeFallbackTitle is OpenCode's placeholder for an untitled session, which the TUI also titles "OpenCode".
var openCodeFallbackTitle = regexp.MustCompile(`^(New session|Child session) - \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

type openCodeRouteWatch struct {
	mu    sync.Mutex
	lanes map[string]openCodeRouteLane // by instance id

	pane   func(inst *Instance) (tmux.PaneInfo, bool)
	reopen func(inst *Instance) error
}

type openCodeRouteLane struct {
	servicePID int  // the service the lane last showed its session under; 0 is unknown
	homeSeen   bool // the previous sweep saw the home title too
}

var sharedOpenCodeRouteWatch = newOpenCodeRouteWatch()

func newOpenCodeRouteWatch() *openCodeRouteWatch {
	return &openCodeRouteWatch{
		lanes:  map[string]openCodeRouteLane{},
		pane:   freshOpenCodePaneInfo,
		reopen: (*Instance).Restart,
	}
}

// ReopenLostOpenCodeRoutes restarts each bound 2.x lane whose TUI left its session when the shared service restarted.
func ReopenLostOpenCodeRoutes(instances []*Instance) {
	sharedOpenCodeRouteWatch.sweep(instances)
}

func (w *openCodeRouteWatch) sweep(instances []*Instance) {
	pid := liveOpenCodeServicePID()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, inst := range instances {
		if !inst.watchesOpenCodeRoute() {
			continue
		}
		info, ok := w.pane(inst)
		if !ok {
			continue
		}
		lane := w.lanes[inst.ID]
		if info.Title != openCodeHomeTitle {
			w.lanes[inst.ID] = openCodeRouteLane{servicePID: pid}
			continue
		}
		// Home under the service that last showed the session is the user's own navigation.
		if pid == 0 || lane.servicePID == pid {
			continue
		}
		if !lane.homeSeen {
			w.lanes[inst.ID] = openCodeRouteLane{servicePID: lane.servicePID, homeSeen: true}
			continue
		}
		if !inst.boundOpenCodeSessionTitled() {
			continue
		}
		sessionLog.Warn("opencode_route_lost_reopen",
			slog.String("instance_id", inst.ID),
			slog.String("session_id", inst.OpenCodeSessionID))
		if err := w.reopen(inst); err != nil {
			sessionLog.Warn("opencode_route_reopen_failed",
				slog.String("instance_id", inst.ID),
				slog.String("error", err.Error()))
			continue
		}
		w.lanes[inst.ID] = openCodeRouteLane{servicePID: pid}
	}
}

func (i *Instance) watchesOpenCodeRoute() bool {
	i.mu.RLock()
	bound := i.Tool == "opencode" && i.OpenCodeSessionID != ""
	started := i.lastStartTime
	i.mu.RUnlock()
	return bound && !started.IsZero() && time.Since(started) > openCodeRouteSettle && i.openCodeRejectsV1LaunchFlags()
}

// freshOpenCodePaneInfo rejects a snapshot taken before the lane's last start: it describes the previous process.
func freshOpenCodePaneInfo(inst *Instance) (tmux.PaneInfo, bool) {
	if inst.tmuxSession == nil {
		return tmux.PaneInfo{}, false
	}
	info, at, ok := tmux.GetCachedPaneInfoSnapshot(inst.tmuxSession.Name)
	inst.mu.RLock()
	started := inst.lastStartTime
	inst.mu.RUnlock()
	return info, ok && !info.Dead && !at.Before(started)
}

func (i *Instance) boundOpenCodeSessionTitled() bool {
	binary, ok := i.openCodeLaunchBinary()
	if !ok {
		return false
	}
	output, err := runOpenCodeServiceCall(binary, i.ProjectPath, "session.get", i.OpenCodeSessionID, "")
	if err != nil {
		return false
	}
	var got struct {
		Data struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	if json.Unmarshal(output, &got) != nil {
		return false
	}
	return got.Data.Title != "" && !openCodeFallbackTitle.MatchString(got.Data.Title)
}

func liveOpenCodeServicePID() int {
	reg, ok := readOpenCodeServiceRegistration()
	if !ok || !procowner.Alive(reg.PID) {
		return 0
	}
	return reg.PID
}
