package tmux

import (
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
)

// These fire on every status pass of every row, so each must reach the log as one event_summary per window.
var perPassStatusEvents = []string{
	"title_done_fallthrough",
	"needs_busy_check",
	"busy_no_spinner",
	"prompt_detected_waiting",
}

func TestPerCaptureStatusEventsAreAggregated(t *testing.T) {
	dir := t.TempDir()
	logging.Shutdown()
	logging.Init(logging.Config{
		Debug:                 true,
		LogDir:                dir,
		Level:                 "debug",
		AggregateIntervalSecs: 1,
	})
	defer logging.Shutdown()

	s := startPaneWithContent(t, "status-debug-aggregate", realisticClaudeDoneContent, "claude", "Cooked for 32s")
	if out, err := s.tmuxCmd("select-pane", "-t", s.Name+":", "-T", "✳ Done").CombinedOutput(); err != nil {
		t.Fatalf("set pane title: %v: %s", err, out)
	}

	const polls = 3
	for poll := 1; poll <= polls; poll++ {
		RefreshPaneInfoCache()
		if info, ok := GetCachedPaneInfo(s.Name); !ok || AnalyzePaneTitle(info.Title, info.CurrentCommand) != TitleStateDone {
			t.Fatalf("precondition: pane title not seen as done (title=%q, cached=%v)", info.Title, ok)
		}
		s.invalidateCache()
		s.mu.Lock()
		if s.stateTracker != nil {
			s.stateTracker.lastActivityTimestamp = 0
		}
		s.mu.Unlock()
		status, err := s.GetStatus()
		if err != nil {
			t.Fatalf("poll %d: GetStatus: %v", poll, err)
		}
		if status != "waiting" {
			t.Fatalf("poll %d = %q, want waiting", poll, status)
		}
	}

	summaries := map[string]float64{}
	allSummarized := func() bool {
		for _, event := range perPassStatusEvents {
			if summaries[event] < polls {
				return false
			}
		}
		return true
	}
	for deadline := time.Now().Add(3 * time.Second); !allSummarized() && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
		clear(summaries)
		for _, r := range tmuxFilterRec(tmuxReadJSONLines(t, dir), "event_summary") {
			event, _ := r["event"].(string)
			count, _ := r["count"].(float64)
			summaries[event] += count
		}
	}

	records := tmuxReadJSONLines(t, dir)
	for _, event := range perPassStatusEvents {
		if hits := tmuxFilterRec(records, event); len(hits) > 0 {
			t.Errorf("%s logged %d per-call records, want none", event, len(hits))
		}
		if summaries[event] < polls {
			t.Errorf("%s event_summary count = %v, want >= %d", event, summaries[event], polls)
		}
	}
}
