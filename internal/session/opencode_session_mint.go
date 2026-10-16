package session

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 2.x creates a session under the `-s <id>` it was given, so a fresh lane mints its id like --session-id.

const openCodeSessionIDChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var openCodeSessionIDClock struct {
	sync.Mutex
	lastMillis int64
	counter    uint64
}

// newOpenCodeSessionID returns an id in OpenCode's own format
// (packages/schema/src/identifier.ts, descending): "ses_", the inverted
// millisecond clock and counter as 12 hex digits, then 14 random base62
// characters. Listings order by id, so a minted session sorts newest first
// like one the TUI created.
func newOpenCodeSessionID(now time.Time) string {
	millis := now.UnixMilli()
	openCodeSessionIDClock.Lock()
	if millis != openCodeSessionIDClock.lastMillis {
		openCodeSessionIDClock.lastMillis = millis
		openCodeSessionIDClock.counter = 0
	}
	openCodeSessionIDClock.counter++
	current := uint64(millis)*0x1000 + openCodeSessionIDClock.counter
	openCodeSessionIDClock.Unlock()

	random := make([]byte, 14)
	_, _ = rand.Read(random)
	var id strings.Builder
	id.WriteString("ses_")
	fmt.Fprintf(&id, "%012x", ^current&0xffffffffffff)
	for _, b := range random {
		id.WriteByte(openCodeSessionIDChars[int(b)%len(openCodeSessionIDChars)])
	}
	return id.String()
}

// bindMintedOpenCodeSession gives an unbound 2.x lane the id its next launch
// creates. A lane restarted before its first prompt keeps the same id.
func (i *Instance) bindMintedOpenCodeSession() {
	i.mu.Lock()
	if i.OpenCodeSessionID != "" {
		i.mu.Unlock()
		return
	}
	id := newOpenCodeSessionID(time.Now())
	i.setOpenCodeSessionLocked(id)
	i.mu.Unlock()
	sessionLog.Info("opencode_session_minted",
		slog.String("instance_id", i.ID),
		slog.String("session_id", id))
}

// openCodeBoundOnSharedService reports a bound 2.x lane, for which async detection has nothing to find.
func (i *Instance) openCodeBoundOnSharedService() bool {
	i.mu.RLock()
	bound := i.OpenCodeSessionID != "" && i.OpenCodePort == 0
	i.mu.RUnlock()
	return bound && i.openCodeRejectsV1LaunchFlags()
}

// openCodeV2LaunchesBare reports a launch with no id, model, agent or session
// mode, the one case where the 2.x TUI would otherwise open an unbound session.
func (i *Instance) openCodeV2LaunchesBare() bool {
	opts := i.GetOpenCodeOptions()
	if opts == nil {
		cfg, _ := LoadUserConfig()
		opts = NewOpenCodeOptions(cfg)
	}
	return opts.Model == "" && opts.Agent == "" && opts.SessionMode != "continue" && opts.SessionMode != "resume"
}
