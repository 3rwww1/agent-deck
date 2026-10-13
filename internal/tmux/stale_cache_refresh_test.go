package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const staleRows = 10

// installStaleCacheFake serves a fleet of staleRows live sessions and logs every call.
func installStaleCacheFake(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("STALE_FIXTURE", dir)
	var windows, panes strings.Builder
	for k := 0; k < staleRows; k++ {
		fmt.Fprintf(&windows, "stale-%d|100|0|@%d|w\n", k, k)
		fmt.Fprintf(&panes, "stale-%d|claude|0|0|0|title\n", k)
	}
	writeStaleFixture(t, dir, "windows", windows.String())
	writeStaleFixture(t, dir, "panes", panes.String())
	writeFakeTmux(t, dir, `printf '%s\n' "$*" >> "$STALE_FIXTURE/calls"
[ -f "$STALE_FIXTURE/down" ] && { echo 'no server running' >&2; exit 1; }
[ -f "$STALE_FIXTURE/slow" ] && sleep 0.3
case "$*" in
*list-windows*) [ -f "$STALE_FIXTURE/slow-listing" ] && sleep 2.3; cat "$STALE_FIXTURE/windows";;
*"list-panes -a"*) cat "$STALE_FIXTURE/panes";;
*list-panes*) echo 0;;
*display-message*) echo 100;;
esac
exit 0
`)
	resetStaleCachesForTest(t)
	return dir
}

func writeStaleFixture(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resetStaleCachesForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		sessionCacheMu.Lock()
		sessionCacheData, sessionCacheTime, sessionCacheStarted, sessionCacheStoredAt = nil, time.Time{}, time.Time{}, time.Time{}
		sessionCacheMu.Unlock()
		paneCacheMu.Lock()
		paneCacheData, paneCacheTime, paneCacheStarted, paneCacheStoredAt = nil, time.Time{}, time.Time{}, time.Time{}
		paneCacheMu.Unlock()
		resetCacheRefreshGatesForTest()
	}
	reset()
	t.Cleanup(reset)
}

// expireSharedCaches ages both caches past their TTL, as a long pass does.
func expireSharedCaches() {
	sessionCacheMu.Lock()
	sessionCacheTime = time.Now().Add(-time.Hour)
	sessionCacheMu.Unlock()
	paneCacheMu.Lock()
	paneCacheTime = time.Now().Add(-time.Hour)
	paneCacheMu.Unlock()
}

// callCounts reads the fake's log, truncates it, and counts calls by verb.
func callCounts(t *testing.T, dir string) map[string]int {
	t.Helper()
	path := filepath.Join(dir, "calls")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		for _, verb := range []string{"list-windows", "list-panes -a", "has-session", "display-message"} {
			if strings.Contains(line, verb) {
				counts[verb]++
			}
		}
		if strings.Contains(line, "list-panes -t") {
			counts["list-panes -t"]++
		}
	}
	return counts
}

func staleSession(k int) *Session {
	return &Session{Name: fmt.Sprintf("stale-%d", k), SocketName: DefaultSocketName()}
}

// The per-row reads a status pass makes on a live row.
func readLiveRow(s *Session) {
	for i := 0; i < 3; i++ {
		_ = s.Exists()
	}
	_, _ = s.GetWindowActivity()
	_ = s.IsPaneDead()
}

func TestStaleCacheRefresh_LiveRowsAfterExpiryShareOneRefresh(t *testing.T) {
	dir := installStaleCacheFake(t)
	RefreshSessionCache()
	RefreshPaneInfoCache()
	expireSharedCaches()
	_ = callCounts(t, dir)

	for k := 0; k < staleRows; k++ {
		readLiveRow(staleSession(k))
	}

	got := callCounts(t, dir)
	t.Logf("%d live rows after cache expiry: %v", staleRows, got)
	want := map[string]int{"list-windows": 1, "list-panes -a": 1}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v (one shared refresh per cache, no per-row probe)", got, want)
	}
}

func TestStaleCacheRefresh_ColdProcessKeepsDirectProbes(t *testing.T) {
	dir := installStaleCacheFake(t)
	s := staleSession(0)
	if !s.Exists() {
		t.Fatal("Exists() = false on a live session")
	}
	got := callCounts(t, dir)
	if got["list-windows"] != 0 || got["has-session"] != 1 {
		t.Fatalf("cold process calls = %v, want one has-session and no list-windows", got)
	}
}

func TestStaleCacheRefresh_DeadServerTriesOncePerTTL(t *testing.T) {
	dir := installStaleCacheFake(t)
	RefreshSessionCache()
	RefreshPaneInfoCache()
	writeStaleFixture(t, dir, "down", "")
	expireSharedCaches()
	_ = callCounts(t, dir)

	start := time.Now()
	for k := 0; k < staleRows; k++ {
		readLiveRow(staleSession(k))
	}
	elapsed := time.Since(start)

	got := callCounts(t, dir)
	t.Logf("%d rows with the server down in %s: %v", staleRows, elapsed, got)
	if limit := int(elapsed/sessionCacheTTL) + 1; got["list-windows"] > limit || got["list-panes -a"] > limit {
		t.Fatalf("calls = %v in %s, want at most %d of each listing (one per TTL)", got, elapsed, limit)
	}
}

func TestStaleCacheRefresh_ConcurrentReadersSingleFlight(t *testing.T) {
	dir := installStaleCacheFake(t)
	RefreshSessionCache()
	RefreshPaneInfoCache()
	writeStaleFixture(t, dir, "slow", "")
	expireSharedCaches()
	_ = callCounts(t, dir)

	var wg sync.WaitGroup
	for k := 0; k < staleRows; k++ {
		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			readLiveRow(s)
		}(staleSession(k))
	}
	wg.Wait()

	got := callCounts(t, dir)
	if got["list-windows"] != 1 || got["list-panes -a"] != 1 || got["has-session"] != 0 {
		t.Fatalf("concurrent readers calls = %v, want one refresh of each cache", got)
	}
}

// One timeout still reads alive, so one sweep runs against a hung server: bound its listings.
func TestStaleCacheRefresh_HungServerSweepListings(t *testing.T) {
	dir := installStaleCacheFake(t)
	RefreshSessionCache()
	RefreshPaneInfoCache()
	restorePoll, restoreProbe := tmuxPollTimeout, hasSessionProbeTimeout
	tmuxPollTimeout, hasSessionProbeTimeout = 200*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { tmuxPollTimeout, hasSessionProbeTimeout = restorePoll, restoreProbe })
	ResetServerAliveCacheForTest(t)
	writeFakeTmux(t, dir, `printf '%s\n' "$*" >> "$STALE_FIXTURE/calls"
exec sleep 30
`)
	if !IsServerAlive() {
		t.Fatal("one timeout declared the server dead")
	}
	_ = callCounts(t, dir)

	start := time.Now()
	RefreshSessionCache()
	RefreshPaneInfoCache()
	for k := 0; k < staleRows; k++ {
		readLiveRow(staleSession(k))
	}
	elapsed := time.Since(start)

	got := callCounts(t, dir)
	t.Logf("one sweep of %d rows on a hung server took %s: %v", staleRows, elapsed, got)
	if limit := int(elapsed/sessionCacheTTL) + 1; got["list-windows"] > limit {
		t.Fatalf("list-windows = %d in %s, want at most %d (one per TTL)", got["list-windows"], elapsed, limit)
	}
	if limit := int(elapsed/paneCacheTTL) + 1; got["list-panes -a"] > limit {
		t.Fatalf("list-panes -a = %d in %s, want at most %d (one per TTL)", got["list-panes -a"], elapsed, limit)
	}
}

func TestStaleCacheRefresh_IsolatedSocketSkipsSharedRefresh(t *testing.T) {
	dir := installStaleCacheFake(t)
	RefreshSessionCache()
	RefreshPaneInfoCache()
	expireSharedCaches()
	_ = callCounts(t, dir)

	s := &Session{Name: "stale-0", SocketName: "t8-isolated"}
	readLiveRow(s)
	_, _ = s.GetStatus()

	got := callCounts(t, dir)
	if got["list-windows"] != 0 || got["list-panes -a"] != 0 {
		t.Fatalf("isolated-socket row calls = %v, want no default-server listing", got)
	}
}

// A gated refresh and the tick's refresh can overlap: the listing that began last must win.
func TestStaleCacheRefresh_OlderListingNeverOverwritesNewer(t *testing.T) {
	resetStaleCachesForTest(t)
	older, newer := time.Now(), time.Now().Add(time.Millisecond)
	storeSessionCache(newer, map[string]int64{"fresh": 1}, nil)
	storeSessionCache(older, map[string]int64{"killed": 1}, nil)
	storePaneCache(newer, map[string]PaneInfo{"fresh": {}}, nil)
	storePaneCache(older, map[string]PaneInfo{"killed": {}}, nil)
	if exists, valid := sessionExistsFromCache("killed"); !valid || exists {
		t.Fatalf("older session listing won: exists=%v valid=%v", exists, valid)
	}
	if _, ok := GetCachedPaneInfo("killed"); ok {
		t.Fatal("older pane listing won")
	}
}

// A refresh that fails after a newer-started one stored keeps that listing and arms no backoff.
func TestStaleCacheRefresh_FailureKeepsListingStoredMeanwhile(t *testing.T) {
	dir := installStaleCacheFake(t)
	RefreshSessionCache()
	writeStaleFixture(t, dir, "down", "")
	sessionCacheMu.Lock()
	sessionCacheStoredAt = time.Now().Add(time.Hour)
	sessionCacheMu.Unlock()
	RefreshSessionCache()
	if exists, valid := sessionExistsFromCache("stale-0"); !valid || !exists {
		t.Fatalf("failed refresh wiped a listing stored while it ran: exists=%v valid=%v", exists, valid)
	}
	if sessionCacheGate.backingOff() {
		t.Fatal("failed refresh armed the backoff over a newer listing")
	}
}

// A session registered after a listing began survives that listing.
func TestStaleCacheRefresh_RegisteredSessionSurvivesOlderListing(t *testing.T) {
	resetStaleCachesForTest(t)
	began := time.Now()
	registerSessionInCache("new-one")
	storeSessionCache(began, map[string]int64{"other": 1}, nil)
	sessionCacheMu.RLock()
	_, kept := sessionCacheData["new-one"]
	sessionCacheMu.RUnlock()
	if !kept {
		t.Fatal("a listing taken before the session existed dropped it")
	}
}

// A refresh that fails after a session was registered keeps that session.
func TestStaleCacheRefresh_FailedRefreshKeepsRegisteredSession(t *testing.T) {
	dir := installStaleCacheFake(t)
	RefreshSessionCache()
	writeStaleFixture(t, dir, "down", "")
	writeFakeTmux(t, dir, `sleep 0.3; echo 'no server running' >&2; exit 1
`)
	done := make(chan struct{})
	go func() { RefreshSessionCache(); close(done) }()
	time.Sleep(100 * time.Millisecond)
	registerSessionInCache("new-one")
	<-done
	sessionCacheMu.RLock()
	_, kept := sessionCacheData["new-one"]
	sessionCacheMu.RUnlock()
	if !kept {
		t.Fatal("a refresh that began before the session was registered wiped it on failure")
	}
}

// A listing slower than the TTL but under the poll timeout must not be stale when it lands.
func TestStaleCacheRefresh_SlowListingServesLaterRows(t *testing.T) {
	dir := installStaleCacheFake(t)
	RefreshSessionCache()
	RefreshPaneInfoCache()
	expireSharedCaches()
	_ = callCounts(t, dir)
	writeStaleFixture(t, dir, "slow-listing", "")

	for k := 0; k < 3; k++ {
		_ = staleSession(k).Exists()
	}
	if got := callCounts(t, dir); got["list-windows"] != 1 || got["has-session"] != 0 {
		t.Fatalf("calls = %v, want one list-windows and no has-session: a slow listing was stale on arrival", got)
	}
}
