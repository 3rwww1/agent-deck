package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestBindOpenCodeSession(t *testing.T) {
	for _, tc := range []struct {
		name              string
		create            bool
		identity          bool
		registered        bool
		instructionStatus int
	}{
		{"fresh", true, true, false, http.StatusNoContent},
		{"registered service", false, true, true, http.StatusNoContent},
		{"opt-out", false, false, false, http.StatusNoContent},
		{"instruction failure", false, true, false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identityTestEnv(t)
			inst := identityTestInstance("opencode")
			t.Setenv("AGENTDECK_INSTANCE_ID", inst.ID)
			t.Setenv("AGENTDECK_TITLE", inst.Title)
			t.Setenv(IdentityFileEnv, "")
			if tc.identity {
				_, file, ok := inst.ensureIdentityFile()
				if !ok {
					t.Fatal("fixture identity not written")
				}
				t.Setenv(IdentityFileEnv, file)
			}
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != "opencode" || password != "fixture-password" {
					http.Error(w, "authentication required", http.StatusUnauthorized)
					return
				}
				requests = append(requests, r.Method+" "+r.URL.Path)
				if r.Method == http.MethodGet {
					if tc.create {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					fmt.Fprint(w, `{"data":{"id":"ses_SELECTED"}}`)
					return
				}
				if r.Method == http.MethodPost {
					var input struct {
						ID    string `json:"id"`
						Title string `json:"title"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if input.ID != "ses_SELECTED" || input.Title != inst.Title {
						t.Error("creation replaced the selected session identity")
					}
					fmt.Fprint(w, `{"data":{"id":"ses_SELECTED"}}`)
					return
				}
				if strings.Contains(r.URL.Path, "/instructions/") {
					w.WriteHeader(tc.instructionStatus)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			registerOpenCodeService(t, srv.URL, os.Getpid(), "fixture-password")
			server := srv.URL
			if tc.registered {
				server = ""
			}
			err := BindOpenCodeSession(context.Background(), server, "ses_SELECTED", tc.create)
			if tc.instructionStatus != http.StatusNoContent {
				if err == nil {
					t.Fatal("rejected instruction binding was ignored")
				}
				if strings.Contains(strings.Join(requests, "\n"), "/environment") {
					t.Fatal("instruction failure still replaced shell environment")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"GET /api/session/ses_SELECTED"}
			if tc.create {
				want = append(want, "POST /api/session")
			}
			method := http.MethodPut
			if !tc.identity {
				method = http.MethodDelete
			}
			want = append(want,
				method+" /api/experimental/session/ses_SELECTED/instructions/entries/agent-deck-identity",
				"PUT /api/session/ses_SELECTED/environment")
			if !reflect.DeepEqual(requests, want) {
				t.Errorf("requests = %v, want %v", requests, want)
			}
		})
	}
}

func TestOpenCodeConfigWithoutIdentity(t *testing.T) {
	for _, raw := range []string{
		`{ "model": "user/model", "instructions": ["/user/rules.md"] }`,
		`{"instructions":["/home/agent-deck/runtime/identity/user-rules.md"]}`,
		`{"instructions":null}`,
		`{"model":"user/model"}`,
		`invalid JSON`,
	} {
		if got := openCodeConfigWithoutIdentity(raw); got != raw {
			t.Errorf("unmanaged config changed: %q", got)
		}
	}
}
