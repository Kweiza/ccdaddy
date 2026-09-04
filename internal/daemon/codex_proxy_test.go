package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/codexproxy"
	"github.com/Kweiza/ccdaddy/internal/codexusage"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func TestTheRankingTheLaneRecordedIsWhatTheProxyReads(t *testing.T) {
	e := NewEngine()
	if got := e.codexRanked(); len(got) != 0 {
		t.Fatalf("codexRanked() = %v before any lane tick, want nothing", got)
	}
	e.SetCodexRanked([]string{"uuid-a", "uuid-b"})
	got := e.codexRanked()
	if len(got) != 2 || got[0] != "uuid-a" || got[1] != "uuid-b" {
		t.Fatalf("codexRanked() = %v, want the recorded order", got)
	}
	// The proxy reads this from a request goroutine while the lane writes it
	// from the tick, so it must hand out a copy.
	got[0] = "clobbered"
	if again := e.codexRanked(); again[0] != "uuid-a" {
		t.Fatalf("codexRanked() = %v after a caller wrote into the last result", again)
	}
}

func TestAHarvestedReadingIsHandedToTheLaneExactlyOnce(t *testing.T) {
	e := NewEngine()
	if _, ok := e.CodexSample("uuid-a"); ok {
		t.Fatal("CodexSample() reported a reading nothing harvested")
	}
	snap := &usage.Snapshot{}
	e.harvestCodexSample("uuid-a", snap)

	got, ok := e.CodexSample("uuid-a")
	if !ok || got != snap {
		t.Fatalf("CodexSample() = (%v, %v), want the harvested reading", got, ok)
	}
	// Once committed it must not be committed again, or one reading would be
	// written into the cache on every tick until another replaced it.
	if _, ok := e.CodexSample("uuid-a"); ok {
		t.Fatal("the same reading was handed out twice")
	}
}

// The lane's ranking is what the proxy serves a new thread from, and the two
// live in different packages joined by exactly this function.
func TestRankedUUIDsFlattensThePoolBestFirst(t *testing.T) {
	ev := switcher.Evaluation{Plan: strategy.Plan{Result: strategy.Result{
		Order: []strategy.Ranked{{UUID: "uuid-a"}, {UUID: "uuid-b"}},
	}}}
	got := rankedUUIDs(ev)
	if len(got) != 2 || got[0] != "uuid-a" || got[1] != "uuid-b" {
		t.Fatalf("rankedUUIDs() = %v, want [uuid-a uuid-b] in that order", got)
	}
	if got := rankedUUIDs(switcher.Evaluation{}); len(got) != 0 {
		t.Fatalf("rankedUUIDs() = %v for an evaluation that never ranked, want nothing", got)
	}
}

// The flattener above is joined to the proxy by one line in Tick, and that line
// was what nothing tested: deleting `e.SetCodexRanked(rankedUUIDs(codexEv))`
// left the whole daemon package green, while codexRanked() came back empty
// forever and the proxy fell through to the store's row order -- so a user with
// three codex accounts got new threads, and 429 replays, on whichever account
// accounts.toml happened to list first rather than on the one the lane ranks
// best. That is the disagreement between the two halves of ccdad that the line
// exists to prevent.
//
// It takes TWO ticks, and that is not padding. The lane ranks on the cache as
// it stands and dispatches the polls whose answers the next tick will rank, so
// the first tick's evaluation has no readings in it and ranks nothing at all --
// an assertion made after one tick reads [] whether the wiring is there or not.
func TestTheTickHandsTheLanesRankingToTheProxy(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")
	seedCodexAccount(t, "cx-2")

	e := codexEngine(t, codexTokensAreFine,
		func(_ context.Context, token, _ string) (*usage.Snapshot, codexusage.Identity, error) {
			if token == "AT-cx-1" {
				return codexSnapshot(90), codexusage.Identity{}, nil
			}
			return codexSnapshot(10), codexusage.Identity{}, nil
		})
	tick(t, e) // fills the cache
	tick(t, e) // ranks on it

	got := e.codexRanked()
	if len(got) != 2 || got[0] != "cx-2" || got[1] != "cx-1" {
		t.Fatalf("codexRanked() = %v after a tick that ranked two codex accounts, want [cx-2 cx-1]: cx-1 is 90%% spent and cx-2 is 10%%, so the proxy must start a new thread on cx-2", got)
	}
}

func TestAHarvestWithNothingInItIsIgnored(t *testing.T) {
	e := NewEngine()
	e.harvestCodexSample("", &usage.Snapshot{})
	e.harvestCodexSample("uuid-a", nil)
	if _, ok := e.CodexSample("uuid-a"); ok {
		t.Fatal("a nil reading was recorded")
	}
	if _, ok := e.CodexSample(""); ok {
		t.Fatal("a reading with no account was recorded")
	}
}

// plantDeadLaunch writes the two files a codex launcher that was killed leaves
// behind: a record and a lock nothing holds.
func plantDeadLaunch(t *testing.T, root, name string) (lockPath, jsonPath string) {
	t.Helper()
	dir := filepath.Join(root, "codex", "launches")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath = filepath.Join(dir, name+".lock")
	jsonPath = filepath.Join(dir, name+".json")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, []byte(`{"pin":"","startedAt":"2026-09-02T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return lockPath, jsonPath
}

func TestDeadLaunchRecordsAreReapedButNotOnEveryTick(t *testing.T) {
	root := isolate(t)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	e := NewEngine()
	e.Now = func() time.Time { return now }

	lock, doc := plantDeadLaunch(t, root, "aaaa1111")
	e.reapCodexLaunches()
	for _, p := range []string{lock, doc} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep", filepath.Base(p))
		}
	}

	// The tick runs about once a second. A sweep that ran every time would stat
	// and try-lock every live codex session's record 86,400 times a day.
	lock, doc = plantDeadLaunch(t, root, "bbbb2222")
	e.reapCodexLaunches()
	if _, err := os.Stat(doc); err != nil {
		t.Fatalf("a second sweep ran inside the interval: %v", err)
	}

	now = now.Add(codexReapInterval + time.Second)
	e.reapCodexLaunches()
	for _, p := range []string{lock, doc} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep after the interval passed", filepath.Base(p))
		}
	}
}

func TestReapingAStoreWithNoLaunchesIsSilent(t *testing.T) {
	isolate(t)
	e := NewEngine()
	var lines []string
	e.Log = func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	e.reapCodexLaunches()
	if len(lines) != 0 {
		t.Fatalf("a store with no launches logged %v", lines)
	}
}

// freePort names a port nothing is listening on.
//
// It binds and immediately closes, which is the only way to learn a free port
// number from the kernel. There is no TIME_WAIT to worry about: the socket
// never accepted a connection, so the port is bindable again at once.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// closeProxy releases the listener startCodexProxy took, so a package whose
// tests each start a proxy does not run out of descriptors.
func closeProxy(t *testing.T, p Proxy) {
	t.Helper()
	srv, ok := p.(*codexproxy.Server)
	if !ok {
		t.Fatalf("startCodexProxy() returned %T, want *codexproxy.Server", p)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTheConfiguredProxyPortIsTheOneBound(t *testing.T) {
	isolate(t)
	want := freePort(t)
	writeConfig(t, fmt.Sprintf("[codex]\nproxy_port = %d\n", want))

	e := NewEngine()
	proxy, err := e.startCodexProxy(context.Background())
	if err != nil {
		t.Fatalf("startCodexProxy() error = %v, want nil", err)
	}
	defer closeProxy(t, proxy)
	if proxy.Port() != want {
		t.Fatalf("the proxy bound %d; [codex].proxy_port asked for %d", proxy.Port(), want)
	}
}

func TestTheConfiguredCrossAccountReplayReachesTheProxy(t *testing.T) {
	root := isolate(t)
	e := NewEngine()
	if cfg, err := e.codexProxyConfig(root); err != nil || cfg.CrossAccountReplay {
		t.Fatalf("codexProxyConfig() = (CrossAccountReplay %v, %v) for a store with no config file, want it off", cfg.CrossAccountReplay, err)
	}
	writeConfig(t, "[codex]\ncross_account_replay = true\n")
	cfg, err := e.codexProxyConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CrossAccountReplay {
		t.Fatal("the proxy was built with cross-account replay off after config.toml turned it on; a mid-thread 429 would be returned rather than replayed, forever")
	}
}

// A port the user pinned that will not bind is the ONE thing that stops the
// daemon: serving on a different port would leave every codex session pointed
// at a port nothing answers, and codex's symptom for that is an endless
// reconnect rather than an error. It is checked here rather than only in
// codexproxy because the branch is reached only if the daemon carries the
// "config" source all the way from config.toml to the bind.
func TestAConfiguredPortThatIsHeldStopsTheDaemon(t *testing.T) {
	isolate(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port
	writeConfig(t, fmt.Sprintf("[codex]\nproxy_port = %d\n", port))

	e := NewEngine()
	proxy, err := e.startCodexProxy(context.Background())
	if err == nil {
		closeProxy(t, proxy)
		t.Fatalf("startCodexProxy() bound %d and reported no error, though the configured port %d was held", proxy.Port(), port)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Fatalf("startCodexProxy() error = %v, want it to name the configured port %d", err, port)
	}
}

// Every field of the Config the daemon hands the proxy, asserted here because
// this is the whole seam between the two packages and a bind cannot show it: a
// field left nil or wired to the wrong thing produces a listener that looks
// exactly like a working one.
func TestTheProxyConfigTheDaemonBuildsIsFullyWired(t *testing.T) {
	root := isolate(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{
		Provider: provider.Codex,
		UUID:     "uuid-a", Email: "uuid-a@example.com",
		AddedAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
	}, cclink.Blob{"claudeAiOauth": json.RawMessage(`{"refreshToken":"RT-uuid-a"}`)}); err != nil {
		t.Fatal(err)
	}

	e := NewEngine()
	wireCodex(e)
	var logged []string
	e.Log = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	e.SetCodexRanked([]string{"uuid-a"})

	cfg, err := e.codexProxyConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != root {
		t.Errorf("Root = %q, want %q", cfg.Root, root)
	}
	if cfg.Version != buildinfo.Version {
		t.Errorf("Version = %q, want %q, which is what the health route reports", cfg.Version, buildinfo.Version)
	}
	if cfg.PortSource != "derived" {
		t.Errorf("PortSource = %q, want derived for a store that has recorded nothing and configured nothing", cfg.PortSource)
	}
	if cfg.Port < 20000 || cfg.Port > 31999 {
		t.Errorf("Port = %d, want it derived into 20000-31999", cfg.Port)
	}
	if cfg.Refresher != e.CodexRefresher {
		t.Error("Refresher is not the daemon's one refresher; a burst of 401s would spend the grant more than once")
	}
	if cfg.Book != e.CodexBook {
		t.Error("Book is not the lane's limit book; the proxy and the lane would disagree about which accounts are spent")
	}
	if cfg.Accounts == nil {
		t.Fatal("Accounts is nil; the proxy could not choose an account")
	}
	accounts, err := cfg.Accounts()
	if err != nil || len(accounts) != 1 || accounts[0].UUID != "uuid-a" {
		t.Errorf("Accounts() = (%v, %v), want the one account in this store", accounts, err)
	}
	if cfg.Credentials == nil {
		t.Fatal("Credentials is nil; the proxy could not build a bearer")
	}
	blob, err := cfg.Credentials("uuid-a")
	if err != nil || blob["claudeAiOauth"] == nil {
		t.Errorf("Credentials(\"uuid-a\") = (%v, %v), want the stored blob", blob, err)
	}
	if cfg.RankedEligible == nil {
		t.Fatal("RankedEligible is nil; a new thread would not start on the lane's best account")
	}
	if order := cfg.RankedEligible(); len(order) != 1 || order[0] != "uuid-a" {
		t.Errorf("RankedEligible() = %v, want the order the lane recorded", order)
	}
	if cfg.Harvest == nil {
		t.Fatal("Harvest is nil; a reading taken off a real response would be dropped")
	}
	snap := &usage.Snapshot{}
	cfg.Harvest("uuid-a", snap)
	if got, ok := e.CodexSample("uuid-a"); !ok || got != snap {
		t.Error("Harvest did not reach the engine's sample the lane commits")
	}
	if cfg.Log == nil {
		t.Fatal("Log is nil; the proxy would decide silently")
	}
	cfg.Log("a line from the proxy")
	if len(logged) != 1 || logged[0] != "a line from the proxy" {
		t.Errorf("the daemon log recorded %v, want the proxy's line", logged)
	}
}

func TestAFallbackPortIsNeverRecorded(t *testing.T) {
	root := isolate(t)
	derived, source, err := codexproxy.ResolvePort(root, 0)
	if err != nil || source != "derived" {
		t.Fatalf("ResolvePort() = (%d, %q, %v), want a derived port for a fresh store", derived, source, err)
	}
	held, lerr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(derived)))
	if lerr != nil {
		t.Skipf("something on this machine already holds the derived port %d: %v", derived, lerr)
	}
	defer held.Close()

	e := NewEngine()
	proxy, err := e.startCodexProxy(context.Background())
	if err != nil {
		t.Fatalf("startCodexProxy() error = %v, want a fallback rather than a refusal", err)
	}
	defer closeProxy(t, proxy)
	if !proxy.FellBack() {
		t.Fatalf("the proxy bound %d without falling back, though %d was held", proxy.Port(), derived)
	}
	if _, serr := os.Stat(codexproxy.PortPath(root)); !os.IsNotExist(serr) {
		recorded, _ := os.ReadFile(codexproxy.PortPath(root))
		t.Fatalf("the kernel-chosen fallback port %q was recorded; the next start would resolve it instead of the derived %d", strings.TrimSpace(string(recorded)), derived)
	}
}

func TestTheBoundPortIsRecordedSoTheNextStartComesBackOnIt(t *testing.T) {
	root := isolate(t)
	e := NewEngine()
	proxy, err := e.startCodexProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy(t, proxy)
	if proxy.FellBack() {
		t.Skipf("the derived port was held by something else on this machine; the fallback case is TestAFallbackPortIsNeverRecorded")
	}
	port, source, err := codexproxy.ResolvePort(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if port != proxy.Port() || source != "recorded" {
		t.Fatalf("the next start would resolve (%d, %q); this one bound %d, which is what it must come back on", port, source, proxy.Port())
	}
}

func TestAnUnparseableAccountsFileDoesNotStopTheProxy(t *testing.T) {
	root := isolate(t)
	if err := os.WriteFile(filepath.Join(root, "accounts.toml"), []byte("[[accounts]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewEngine()
	proxy, err := e.startCodexProxy(context.Background())
	if err != nil {
		t.Fatalf("startCodexProxy() error = %v; a store this build cannot parse must not stop the daemon", err)
	}
	closeProxy(t, proxy)
}
