package session

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/procowner"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// An attached 2.x TUI that fails to re-read its session on a reconnect (a
// service restart, stall or overload) falls back to its home screen and stays.

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

	pane    func(inst *Instance) (tmux.PaneInfo, bool)
	watched func(inst *Instance) bool
	reopen  func(inst *Instance) error
}

type openCodeRouteLane struct {
	servicePID int   // the service the lane last showed its session under; 0 is unknown
	outages    int64 // the guard's failed checks at that time
	watched    bool  // someone was on the pane since it last showed its session
	homeSeen   bool  // the previous sweep saw the home title too
}

var sharedOpenCodeRouteWatch = newOpenCodeRouteWatch()

func newOpenCodeRouteWatch() *openCodeRouteWatch {
	return &openCodeRouteWatch{
		lanes:   map[string]openCodeRouteLane{},
		pane:    freshOpenCodePaneInfo,
		watched: openCodePaneWatched,
		reopen:  (*Instance).Restart,
	}
}

// ReopenLostOpenCodeRoutes restarts each bound 2.x lane nobody is looking at whose TUI left its session after a shared service restart or outage.
func ReopenLostOpenCodeRoutes(instances []*Instance) {
	sharedOpenCodeRouteWatch.sweep(instances)
}

func (w *openCodeRouteWatch) sweep(instances []*Instance) {
	pid := liveOpenCodeServicePID()
	outages := sharedOpenCodeService.outages.Load()
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
			w.lanes[inst.ID] = openCodeRouteLane{servicePID: pid, outages: outages, watched: w.watched(inst)}
			continue
		}
		// Only someone on the pane can navigate home: with nobody there, a stall the guard never counted lost the route.
		if pid == 0 || (lane.servicePID == pid && lane.outages == outages && lane.watched) {
			continue
		}
		// A person on the pane may be typing, and a restart would drop the text: what they do next is theirs.
		if w.watched(inst) {
			w.lanes[inst.ID] = openCodeRouteLane{servicePID: pid, outages: outages, watched: true}
			continue
		}
		if !lane.homeSeen {
			lane.homeSeen = true
			w.lanes[inst.ID] = lane
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
		w.lanes[inst.ID] = openCodeRouteLane{servicePID: pid, outages: outages}
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

// openCodePaneWatched counts agent-deck's own preview pipes too, and an unreadable answer counts as watched.
func openCodePaneWatched(inst *Instance) bool {
	if inst.tmuxSession == nil {
		return true
	}
	out, err := tmux.OutputBounded(inst.TmuxSocketName, "display-message", "-p", "-t", inst.tmuxSession.Name, "#{session_attached}")
	return err != nil || strings.TrimSpace(string(out)) != "0"
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
