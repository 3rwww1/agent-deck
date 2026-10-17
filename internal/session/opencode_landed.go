package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	openCodeLandedPageSize  = 50
	openCodeLandedMaxPages  = 5
	openCodeLandedClockSkew = 2 * time.Second
)

// ErrOpenCodeMessagesUnreadable means no later check can succeed: no binary, or the service has no such session.
var ErrOpenCodeMessagesUnreadable = errors.New("opencode session messages unreadable")

var errOpenCodeServiceUnavailable = errors.New("shared opencode service unavailable")

// CanConfirmOpenCodeLanding reports a bound 2.x lane: its messages live on the shared service, not in a transcript.
func (i *Instance) CanConfirmOpenCodeLanding() bool {
	return i.Tool == "opencode" && i.OpenCodeSessionID != "" && i.openCodeRejectsV1LaunchFlags()
}

// FindOpenCodeLanded returns the oldest user message carrying text created at or after notBefore; "" means not yet.
func (i *Instance) FindOpenCodeLanded(text string, notBefore time.Time) (id, ts string, err error) {
	want := strings.Join(strings.Fields(text), " ")
	binary, found := i.openCodeLaunchBinary()
	if !found {
		return "", "", fmt.Errorf("%w: opencode binary not found", ErrOpenCodeMessagesUnreadable)
	}
	// A standalone fallback would start a private server per check, for a send the TUI could not take either.
	svc, ok := sharedOpenCodeService.attach(binary)
	if !ok {
		return "", "", errOpenCodeServiceUnavailable
	}
	since := notBefore.Add(-openCodeLandedClockSkew).UnixMilli()
	query := url.Values{"type": {"user"}, "order": {"desc"}, "limit": {fmt.Sprint(openCodeLandedPageSize)}}
	for page := 0; page < openCodeLandedMaxPages; page++ {
		messages, next, err := fetchOpenCodeMessages(svc, i.OpenCodeSessionID, query)
		if err != nil {
			return "", "", err
		}
		// Pages run newest first, so the last match is the oldest: an earlier send never takes a later copy.
		for _, m := range messages {
			if m.Type == "user" && m.Time.Created >= since && strings.Join(strings.Fields(m.Text), " ") == want {
				id, ts = m.ID, time.UnixMilli(m.Time.Created).UTC().Format(time.RFC3339Nano)
			}
		}
		if next == "" || len(messages) == 0 || messages[len(messages)-1].Time.Created < since {
			break
		}
		query = url.Values{"type": {"user"}, "cursor": {next}}
	}
	return id, ts, nil
}

type openCodeUserMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

func fetchOpenCodeMessages(svc openCodeService, sessionID string, query url.Values) ([]openCodeUserMessage, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := svc.URL + "/api/session/" + url.PathEscape(sessionID) + "/message?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	if svc.Password != "" {
		req.SetBasicAuth("opencode", svc.Password)
	}
	resp, err := openCodeSessionHTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	// A 401 can be a password the service rotated on restart, which the guard picks up on its next check.
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", fmt.Errorf("%w: HTTP %d", ErrOpenCodeMessagesUnreadable, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, "", fmt.Errorf("opencode messages: HTTP %d", resp.StatusCode)
	}
	var page struct {
		Data   []openCodeUserMessage `json:"data"`
		Cursor struct {
			Next string `json:"next"`
		} `json:"cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, "", err
	}
	return page.Data, page.Cursor.Next, nil
}
