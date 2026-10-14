package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// setFakeOpenCodeSessionGet answers `api session.get` with getPayload and any
// other call with listPage, recording each argv under dir/argv.N.
func setFakeOpenCodeSessionGet(t *testing.T, listPage, getPayload string) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\n"+
		"n=0; read -r n < %[1]q/count 2>/dev/null; n=$((n+1)); echo $n > %[1]q/count\n"+
		"printf '%%s\\n' \"$@\" > %[1]q/argv.$n\n"+
		"case \"$*\" in *session.get*) printf '%%s\\n' %[3]q;; *) printf '%%s\\n' %[2]q;; esac\n",
		dir, listPage, getPayload)
	setFakeOpenCodePath(t, script, false)
	configureFakeOpenCode(t)
	return dir
}

func movedSessionPayload(id, directory string) string {
	return fmt.Sprintf(`{"data":{"id":%q,"parentID":null,"location":{"directory":%q},"time":{"created":1000,"updated":2000}}}`, id, directory)
}

// After session_move the bound session no longer lists under ProjectPath; the
// binding must survive, found with one lookup by id.
func TestQueryOpenCodeSession_V2BoundSessionMovedToOtherDirectory(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	projectPath := t.TempDir()
	movedTo := t.TempDir()
	emptyPage := `{"data":[],"cursor":{"previous":null,"next":null}}`
	dir := setFakeOpenCodeSessionGet(t, emptyPage, movedSessionPayload("ses_BOUND", movedTo))

	inst := &Instance{Tool: "opencode", ProjectPath: projectPath, OpenCodeSessionID: "ses_BOUND"}
	if got := inst.queryOpenCodeSession(); got != "ses_BOUND" {
		t.Fatalf("session ID = %q, want ses_BOUND", got)
	}
	if got := fakeOpenCodeCallCount(t, dir); got != 1 {
		t.Fatalf("opencode call count = %d, want 1 (one lookup by id, no paging)", got)
	}
	argv, err := os.ReadFile(filepath.Join(dir, "argv.1"))
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	if want := "api\nsession.get\n--param\nsessionID=ses_BOUND\n"; string(argv) != want {
		t.Fatalf("opencode argv =\n%s\nwant\n%s", argv, want)
	}
}

func TestQueryOpenCodeSession_V2BoundLookupFailureKeepsBinding(t *testing.T) {
	pinOpenCodeMajorVersion(t, 2, true)
	projectPath := t.TempDir()
	sibling := fmt.Sprintf(`{"data":[`+
		`{"id":"ses_SIBLING","parentID":null,"location":{"directory":%q},"time":{"created":5000,"updated":6000}}`+
		`],"cursor":{"previous":null,"next":null}}`, projectPath)
	// 2.0.25 answers an unknown id with exit 1, this JSON on stdout and the HTTP status on stderr.
	notFound := "#!/bin/sh\ncase \"$*\" in *session.get*) echo '{\"_tag\":\"SessionNotFoundError\"}'; " +
		"echo 'HTTP 404 Not Found' >&2; exit 1;; esac\nprintf '%s\\n' '" + sibling + "'\n"
	for name, fake := range map[string]func(t *testing.T){
		"session not found": func(t *testing.T) { setFakeOpenCodePath(t, notFound, false); configureFakeOpenCode(t) },
		"reply for another": func(t *testing.T) {
			setFakeOpenCodeSessionGet(t, sibling, movedSessionPayload("ses_OTHER", projectPath))
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake(t)
			inst := &Instance{Tool: "opencode", ProjectPath: projectPath, OpenCodeSessionID: "ses_GONE"}
			if got := inst.queryOpenCodeSession(); got != "" {
				t.Fatalf("session ID = %q, want empty (no rebind, no sibling adopted)", got)
			}
		})
	}
}
