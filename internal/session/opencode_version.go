package session

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/asheshgoplani/agent-deck/internal/shellwords"
)

// OpenCode 2.x reshaped the root command's flags. It rejects the 1.x
// -m/--model, --agent and --port flags ("Unrecognized flag: --port in command
// opencode") and exits at once, so a session launched with any of them dies
// before the TUI draws (spawn_died_fast). In 2.x model and agent overrides
// are applied through the session API, and events come from a shared service
// rather than a per-TUI --port server.
//
// buildOpenCodeCommand therefore asks the binary a launch will run for its
// version (`opencode --version`, well under the probe timeout) and emits those
// flags only for 1.x. A successful answer is memoised per resolved binary path,
// mtime and size, so an upgrade is re-probed. A binary that cannot be found, or
// whose version does not parse, keeps the 1.x flags: that is the existing
// behaviour.

var openCodeVersionPattern = regexp.MustCompile(`(\d+)\.\d+\.\d+`)

// parseOpenCodeMajorVersion extracts the major version from `opencode
// --version` output: "opencode v2.0.20" on 2.x, a bare "1.14.3" on 1.x.
func parseOpenCodeMajorVersion(out string) (int, bool) {
	m := openCodeVersionPattern.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return major, true
}

type openCodeVersionAnswer struct {
	major   int
	ok      bool
	version string // the full version token, prerelease suffix included, "" when it did not parse
}

// openCodeFullVersionPattern matches the full version token in `opencode --version`
// output, prerelease suffix included: "opencode v2.1.4" -> 2.1.4, the dev channel's
// "0.0.0-dev-42" -> 0.0.0-dev-42.
var openCodeFullVersionPattern = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?`)

// openCodeVersionMemo maps modelProbeKey -> openCodeVersionAnswer.
var openCodeVersionMemo sync.Map

// probeOpenCodeMajorVersion reports the major version of the OpenCode binary a
// local launch of i runs. It is a var so tests can pin a version without an
// installed CLI; TestMain pins "unknown" so the package's tests never exec the
// host's opencode.
var probeOpenCodeMajorVersion = probeInstalledOpenCodeMajorVersion

func probeInstalledOpenCodeMajorVersion(i *Instance) (int, bool) {
	binary, ok := i.openCodeLaunchBinary()
	if !ok {
		return 0, false
	}
	return probeOpenCodeBinaryMajorVersion(binary)
}

func probeOpenCodeBinaryMajorVersion(binary string) (int, bool) {
	answer := probeOpenCodeBinaryVersion(binary)
	return answer.major, answer.ok
}

// probeOpenCodeBinaryVersion memoises the `--version` answer per resolved binary
// path, mtime and size, so an upgrade is re-probed.
func probeOpenCodeBinaryVersion(binary string) openCodeVersionAnswer {
	key, err := modelProbeBinaryKey(binary)
	if err != nil {
		return openCodeVersionAnswer{}
	}
	if cached, ok := openCodeVersionMemo.Load(key); ok {
		return cached.(openCodeVersionAnswer)
	}
	out, err := runModelProbeCommand(key.Path, "--version")
	if err != nil {
		// Not memoised: a cold start can time out once, and remembering that
		// would keep the 1.x flags until the binary changes.
		return openCodeVersionAnswer{}
	}
	var answer openCodeVersionAnswer
	answer.major, answer.ok = parseOpenCodeMajorVersion(string(out))
	answer.version = openCodeFullVersionPattern.FindString(string(out))
	openCodeVersionMemo.Store(key, answer)
	return answer
}

// probeOpenCodeCLIVersion reports the full version the guard's skew check compares
// the service's answer against. A var so tests can pin a version without an
// installed CLI; TestMain pins "" so the package's tests never exec the host's
// opencode.
var probeOpenCodeCLIVersion = openCodeCLIVersion

// runOpenCodeVersionCommand runs the fresh `--version` probe. A var so tests stub
// the exec; TestMain pins it, and useInstalledOpenCodeCLIVersion is the only route
// back to the real command.
var runOpenCodeVersionCommand = runModelProbeCommand

// openCodeCLIVersion answers `--version` fresh on every call, running the command
// by the path found on PATH, links left in place: a version-manager shim is a link
// to the version manager, which dispatches on the name it was called by, so
// following the link would ask the version manager for its own version. No memo:
// the shim file does not change when the tool behind it is upgraded.
func openCodeCLIVersion(binary string) (string, bool) {
	path, err := openCodeCommandPath(binary)
	if err != nil {
		return "", false
	}
	out, err := runOpenCodeVersionCommand(path, "--version")
	if err != nil {
		return "", false
	}
	version := openCodeFullVersionPattern.FindString(string(out))
	return version, version != ""
}

// openCodeCommandPath resolves a configured command the way the pane and the
// service actions run it: the path found on PATH, made absolute, with symlinks
// left in place so a shim keeps the name it was called by.
func openCodeCommandPath(binary string) (string, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", err
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return path, nil
}

// normalizeOpenCodeVersion extracts the comparable version token the CLI side
// parses, so a decorated answer ("v2.1.4") compares equal to the bare one.
func normalizeOpenCodeVersion(version string) string {
	return openCodeFullVersionPattern.FindString(version)
}

// compareOpenCodeVersions orders two versions the way semver does: the numeric
// core field by field, then the prerelease, where a version without one sorts
// above one with it and numeric identifiers sort below alphanumeric ones.
func compareOpenCodeVersions(a, b string) int {
	aCore, aPre := splitOpenCodeVersion(a)
	bCore, bPre := splitOpenCodeVersion(b)
	if c := compareOpenCodeVersionCore(aCore, bCore); c != 0 {
		return c
	}
	switch {
	case aPre == "" && bPre == "":
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	default:
		return compareOpenCodeVersionPrerelease(aPre, bPre)
	}
}

// splitOpenCodeVersion splits a normalized version into its numeric core and
// prerelease.
func splitOpenCodeVersion(version string) (core, pre string) {
	core, pre, _ = strings.Cut(version, "-")
	return core, pre
}

// compareOpenCodeVersionCore compares the dot-separated numeric fields, a missing
// field counting as zero.
func compareOpenCodeVersionCore(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv int
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			return cmp.Compare(av, bv)
		}
	}
	return 0
}

// compareOpenCodeVersionPrerelease compares dot-separated identifiers per semver:
// a longer list of equal identifiers sorts above a shorter one.
func compareOpenCodeVersionPrerelease(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareOpenCodeVersionIdentifier(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}

// compareOpenCodeVersionIdentifier orders one prerelease identifier: numeric ones
// numerically and below the alphanumeric ones. Hyphenated identifiers compare
// their parts, so a dev channel's dev-100 sorts above dev-99, not as text.
func compareOpenCodeVersionIdentifier(a, b string) int {
	as := strings.Split(a, "-")
	bs := strings.Split(b, "-")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareOpenCodeVersionSegment(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}

// compareOpenCodeVersionSegment orders one part of a prerelease identifier:
// numeric parts numerically and below the alphanumeric ones.
func compareOpenCodeVersionSegment(a, b string) int {
	an, aErr := strconv.Atoi(a)
	bn, bErr := strconv.Atoi(b)
	switch {
	case aErr == nil && bErr == nil:
		return cmp.Compare(an, bn)
	case aErr == nil:
		return -1
	case bErr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// openCodeLaunchBinary resolves the executable of the configured [opencode]
// command the way the pane will: the spawn-path prelude's dirs ahead of this
// process's PATH, then any leading PATH= assignment in the command itself
// ($PATH in it expands to the PATH before it). Words are split quote-aware,
// so a quoted path with spaces stays whole. An `env` wrapper is looked
// through: `env [VAR=value...] opencode` runs opencode with those assignments
// applied, PATH included.
func (i *Instance) openCodeLaunchBinary() (string, bool) {
	words, ok := shellwords.Split(i.openCodeForkBinary())
	if !ok {
		return "", false
	}
	pathEnv := os.Getenv("PATH")
	if dirs := i.spawnPathDirs(); len(dirs) > 0 {
		pathEnv = strings.Join(append(dirs, pathEnv), string(os.PathListSeparator))
	}
	name := "opencode"
	wrapped := false
	for _, word := range words {
		if isShellEnvAssignment(word) {
			if value, found := strings.CutPrefix(word, "PATH="); found {
				before := pathEnv
				pathEnv = os.Expand(value, func(v string) string {
					if v == "PATH" {
						return before
					}
					return os.Getenv(v)
				})
			}
			continue
		}
		if !wrapped && filepath.Base(word) == "env" {
			wrapped = true
			continue
		}
		if wrapped && strings.HasPrefix(word, "-") {
			// env options (-i, -u NAME, -S ...) change what follows; don't guess.
			return "", false
		}
		name = word
		break
	}
	return lookPathIn(name, pathEnv)
}

// lookPathIn is exec.LookPath against an explicit PATH value. A name with a
// slash is used as given (a leading ~/ expands to $HOME, as the shell would).
func lookPathIn(name, pathEnv string) (string, bool) {
	if rest, found := strings.CutPrefix(name, "~/"); found {
		name = filepath.Join(os.Getenv("HOME"), rest)
	}
	if strings.Contains(name, "/") {
		return name, isExecutableFile(name)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		if candidate := filepath.Join(dir, name); isExecutableFile(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// openCodeRejectsV1LaunchFlags reports whether this session's OpenCode is 2.x
// or newer. The explicit opencode2 launcher is always v2, even when it cannot
// be probed locally. Other sandboxed and SSH launchers keep the 1.x flags.
func (i *Instance) openCodeRejectsV1LaunchFlags() bool {
	if openCodeExecutableBase(i.openCodeForkBinary()) == "opencode2" {
		return true
	}
	if i.IsSandboxed() || i.IsSSH() {
		return false
	}
	major, ok := probeOpenCodeMajorVersion(i)
	return ok && major >= 2
}

// SupportsOpenCodeQueue reports whether the configured local binary supports
// OpenCode's session-bound native queue API, using the launch version probe.
func (i *Instance) SupportsOpenCodeQueue() bool {
	return i != nil && i.Tool == "opencode" && i.openCodeRejectsV1LaunchFlags()
}
