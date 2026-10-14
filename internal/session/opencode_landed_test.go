package session

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveOpenCodeMessages answers message pages by cursor ("" is the first page) as a healthy shared service.
func serveOpenCodeMessages(t *testing.T, pages map[string]string) *[]string {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); r.URL.Path != "/api/session/ses_BOUND/message" || !ok || user != "opencode" || pass != "secret" {
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		fmt.Fprint(w, pages[r.URL.Query().Get("cursor")])
	}))
	t.Cleanup(srv.Close)
	useOpenCodeStub(t, "#!/bin/sh\nexit 0\n")
	g := useFreshOpenCodeServiceGuard(t)
	g.status = func(string) (openCodeService, error) { return openCodeService{URL: srv.URL, Password: "secret"}, nil }
	g.healthy = func(openCodeService) bool { return true }
	return &queries
}

func userMessage(id, text string, at time.Time) string {
	return fmt.Sprintf(`{"id":%q,"type":"user","text":%q,"time":{"created":%d}}`, id, text, at.UnixMilli())
}

func TestFindOpenCodeLanded_EachSendTakesItsOwnCopy(t *testing.T) {
	sentAt := time.UnixMilli(1_800_000_000_000)
	queries := serveOpenCodeMessages(t, map[string]string{
		"": `{"data":[` + userMessage("msg_B", "continue", sentAt.Add(time.Minute)) + `,` +
			userMessage("msg_OTHER", "another prompt", sentAt.Add(30*time.Second)) + `],"cursor":{"next":"c2"}}`,
		"c2": `{"data":[` + userMessage("msg_A", "  continue\n", sentAt.Add(time.Second)) + `,` +
			userMessage("msg_OLD", "continue", sentAt.Add(-time.Hour)) + `],"cursor":{"next":"c3"}}`,
	})

	inst := &Instance{Tool: "opencode", ProjectPath: t.TempDir(), OpenCodeSessionID: "ses_BOUND"}
	for _, tc := range []struct {
		sent time.Time
		want string
	}{{sentAt, "msg_A"}, {sentAt.Add(59 * time.Second), "msg_B"}} {
		if id, ts, err := inst.FindOpenCodeLanded("continue", tc.sent); err != nil || id != tc.want {
			t.Fatalf("FindOpenCodeLanded(sent %s) = %q, %q, %v; want %s", tc.sent, id, ts, err, tc.want)
		}
	}
	// The first check stops at c2's pre-send row, the second at the first page's: neither reads c3.
	got := strings.Join(*queries, " ")
	if want := "limit=50&order=desc&type=user cursor=c2&type=user limit=50&order=desc&type=user"; got != want {
		t.Fatalf("pages read = %q, want %q", got, want)
	}
}

func TestFindOpenCodeLanded_SaysWhyItCannotRead(t *testing.T) {
	sentAt := time.UnixMilli(1_800_000_000_000)
	serveOpenCodeMessages(t, nil)
	inst := &Instance{Tool: "opencode", ProjectPath: t.TempDir(), OpenCodeSessionID: "ses_GONE"}
	if _, _, err := inst.FindOpenCodeLanded("continue", sentAt); !errors.Is(err, ErrOpenCodeMessagesUnreadable) {
		t.Fatalf("removed session: err = %v, want ErrOpenCodeMessagesUnreadable", err)
	}

	sharedOpenCodeService.status = func(string) (openCodeService, error) { return openCodeService{}, errOpenCodeServiceDisabled }
	sharedOpenCodeService.healthyAt = time.Time{}
	if _, _, err := inst.FindOpenCodeLanded("continue", sentAt); !errors.Is(err, errOpenCodeServiceUnavailable) {
		t.Fatalf("service down: err = %v, want errOpenCodeServiceUnavailable", err)
	}
}

func TestCanConfirmOpenCodeLanding_OnlyBound2xLanes(t *testing.T) {
	for _, tc := range []struct {
		major int
		id    string
		want  bool
	}{{2, "ses_BOUND", true}, {2, "", false}, {1, "ses_BOUND", false}} {
		pinOpenCodeMajorVersion(t, tc.major, true)
		if got := (&Instance{Tool: "opencode", OpenCodeSessionID: tc.id}).CanConfirmOpenCodeLanding(); got != tc.want {
			t.Fatalf("major %d, id %q: CanConfirmOpenCodeLanding = %v, want %v", tc.major, tc.id, got, tc.want)
		}
	}
}
