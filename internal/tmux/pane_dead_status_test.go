package tmux

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParsePaneDeadStatus covers the "#{pane_dead}|#{pane_dead_status}" parsing
// that lets status detection tell a clean one-shot exit (code 0) from a crash.
func TestParsePaneDeadStatus(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantCode int
		wantOK   bool
	}{
		{"clean exit 0", "1|0\n", 0, true},
		{"crash exit 1", "1|1", 1, true},
		{"crash exit 137", "1|137\n", 137, true},
		{"live pane", "0|", 0, false},
		{"live pane with stale status", "0|0", 0, false},
		{"dead pane, no remain-on-exit (empty status)", "1|", 0, false},
		{"dead pane, non-numeric status", "1|foo", 0, false},
		{"malformed, no separator", "1", 0, false},
		{"empty", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, ok := parsePaneDeadStatus(tt.raw)
			if code != tt.wantCode || ok != tt.wantOK {
				t.Errorf("parsePaneDeadStatus(%q) = (%d, %v), want (%d, %v)",
					tt.raw, code, ok, tt.wantCode, tt.wantOK)
			}
		})
	}
}

// With base-index and pane-base-index set to 1 there is no window 0 or pane 0,
// so a probe hardcoding "<session>:0.0" fails and dead panes go undetected.
func TestPaneDeadProbes_NonZeroBaseIndex(t *testing.T) {
	requireTmux(t)
	socket, _ := makeIsolatedServer(t)
	ctl := tmuxCtl(t, socket)
	ctl("set-option", "-g", "base-index", "1")
	ctl("set-option", "-g", "pane-base-index", "1")
	ctl("set-option", "-g", "remain-on-exit", "on")
	ctl("new-session", "-d", "-s", "based", "sh", "-c", "exit 7")
	require.Equal(t, "1.1", ctl("list-panes", "-t", "based", "-F", "#{window_index}.#{pane_index}"))
	waitPaneDead(t, ctl, "based")

	s := &Session{Name: "based", SocketName: socket}
	assertDeadWithExit7(t, s)
}

// A session-only base-index override, with the server default left at 0, must
// not send the probes back to window 0.
func TestPaneDeadProbes_PerSessionBaseIndex(t *testing.T) {
	requireTmux(t)
	socket, _ := makeIsolatedServer(t)
	ctl := tmuxCtl(t, socket)
	ctl("set-option", "-g", "remain-on-exit", "on")
	ctl("new-session", "-d", "-s", "based", "sh")
	ctl("set-option", "-t", "based", "base-index", "1")
	ctl("new-window", "-d", "-t", "based:", "sh", "-c", "exit 7")
	ctl("kill-window", "-t", "based:0")
	require.Equal(t, "1.0", ctl("list-panes", "-t", "based", "-F", "#{window_index}.#{pane_index}"))
	waitPaneDead(t, ctl, "based")

	s := &Session{Name: "based", SocketName: socket}
	assertDeadWithExit7(t, s)
}

// tmux prefix-matches a bare session target, so a probe for a missing session
// must not read the dead pane of another session whose name extends it.
func TestPaneDeadProbes_ExactSessionMatch(t *testing.T) {
	requireTmux(t)
	socket, _ := makeIsolatedServer(t)
	ctl := tmuxCtl(t, socket)
	ctl("set-option", "-g", "remain-on-exit", "on")
	ctl("new-session", "-d", "-s", "basedx", "sh", "-c", "exit 7")
	waitPaneDead(t, ctl, "basedx")

	s := &Session{Name: "based", SocketName: socket}
	assert.False(t, s.IsPaneDead(), "IsPaneDead read basedx")
	_, ok := s.PaneDeadExitStatus()
	assert.False(t, ok, "PaneDeadExitStatus read basedx")
}

func TestPaneDeadExitStatus_OneSpawn(t *testing.T) {
	requireTmux(t)
	socket, _ := makeIsolatedServer(t)
	ctl := tmuxCtl(t, socket)
	ctl("set-option", "-g", "remain-on-exit", "on")
	ctl("new-session", "-d", "-s", "based", "sh", "-c", "exit 7")
	waitPaneDead(t, ctl, "based")

	s := &Session{Name: "based", SocketName: socket}
	before := SubprocessStarts()
	_, _ = s.PaneDeadExitStatus()
	assert.Equal(t, int64(1), SubprocessStarts()-before)
}

func waitPaneDead(t *testing.T, ctl func(args ...string) string, session string) {
	t.Helper()
	require.Eventually(t, func() bool {
		return ctl("list-panes", "-t", "="+session, "-F", "#{pane_dead}") == "1"
	}, 5*time.Second, 20*time.Millisecond)
}

func assertDeadWithExit7(t *testing.T, s *Session) {
	t.Helper()
	assert.True(t, s.IsPaneDead(), "IsPaneDead")
	code, ok := s.PaneDeadExitStatus()
	assert.True(t, ok, "PaneDeadExitStatus ok")
	assert.Equal(t, 7, code, "PaneDeadExitStatus code")
}
