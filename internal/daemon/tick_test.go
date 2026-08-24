package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/oauth"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/tokens"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

var tickEpoch = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

// isolateEngine sandboxes ccdad's store AND Claude Code's credential home. The
// engine WRITES the live credentials file, so a test that skipped the second
// half would log the developer out of Claude Code.
func isolateEngine(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad"))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", claude)
	// THE THIRD CALLER OF switcher.Execute, and the one that was two variables
	// behind. Execute resolves BOTH of Claude Code's credential axes now, so
	// this suite reads about fifteen variables and two absolute paths; without
	// the sandbox a developer who exports any of them gets answers CI never
	// sees, and the failure reads as a flake in the engine.
	for _, v := range identity.AuthEnvironmentVars() {
		t.Setenv(v, "")
	}

	// Whether this machine has Claude Code on it is a fourth axis, and it is the
	// one that would SPAWN something: a tick that decided to probe would re-exec
	// the real binary against a t.TempDir() the framework is about to delete. The
	// default describes a machine with no claude, which is the one answer no host
	// can contradict; a test that means to probe calls stubProbe by name.
	savedLook := lookClaude
	t.Cleanup(func() { lookClaude = savedLook })
	lookClaude = func(string) (string, error) {
		return "", errors.New(`exec: "claude": executable file not found in $PATH`)
	}

	// The two paths no t.Setenv can reach: Claude Code compiles them in as
	// absolute literals outside the home directory. Deliberately not created.
	savedHostToken, savedHostKey := identity.HostOAuthTokenFile, identity.HostAPIKeyFile
	t.Cleanup(func() {
		identity.HostOAuthTokenFile, identity.HostAPIKeyFile = savedHostToken, savedHostKey
	})
	hostRemote := filepath.Join(t.TempDir(), "remote")
	identity.HostOAuthTokenFile = filepath.Join(hostRemote, ".oauth_token")
	identity.HostAPIKeyFile = filepath.Join(hostRemote, ".api_key")
	if got := mustPath(ccpath.CredentialHome()); got != claude {
		t.Fatalf("isolateEngine: CredentialHome() = %q, want %q — refusing to run unsandboxed", got, claude)
	}
}

// It carries user:inference because Claude Code takes a login as a credential
// only when its scopes do: a record without it authenticates nothing, so a
// fixture without it describes a machine with no login at all.
func oauthBlob(refresh string) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"AT-` + refresh + `","refreshToken":"` + refresh + `","expiresAt":` +
			fmt.Sprint(tickEpoch.Add(8*time.Hour).UnixMilli()) +
			`,"scopes":["user:inference","user:profile"]}`)}
}

func seedAccount(t *testing.T, uuid, org string) store.Account {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	// AddedAt is set explicitly and put BEFORE the frozen clock: Cache.Prune
	// drops any reading dated before its account was added, and a real
	// time.Now() stamp would be hours after tickEpoch.
	a := store.Account{
		UUID: uuid, Email: uuid + "@example.com", OrganizationUUID: org,
		AddedAt: tickEpoch.Add(-24 * time.Hour),
	}
	if err := s.Add(a, oauthBlob("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(uuid)
	return got
}

// seedTokenAccount stores an account whose credential Claude Code reads from
// somewhere other than the credentials file. There is no refresh grant behind
// one, so it can never be polled.
func seedTokenAccount(t *testing.T, uuid string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := json.Marshal(cclink.TokenRecord{Kind: cclink.APIKeyKind, Token: "sk-ant-api-" + uuid})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{
		UUID: uuid, Email: uuid + "@example.com", AddedAt: tickEpoch.Add(-24 * time.Hour),
	}, cclink.Blob{cclink.TokenKey: rec}); err != nil {
		t.Fatal(err)
	}
}

func liveAs(t *testing.T, uuid string) {
	t.Helper()
	body := `{"claudeAiOauth":{"accessToken":"AT-RT-` + uuid + `","refreshToken":"RT-` + uuid + `"}}`
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func liveUUID(t *testing.T) string {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	live, err := cclink.Load()
	if err != nil {
		t.Fatal(err)
	}
	a, ok := switcher.AttributeFile(live, s.Accounts(), s.Credentials)
	if !ok {
		return ""
	}
	return a.UUID
}

// snapshotWith builds a reading whose binding window is `used` percent spent.
func snapshotWith(used float64) *usage.Snapshot {
	resets := tickEpoch.Add(time.Hour)
	return &usage.Snapshot{FiveHour: usage.NewWindow(&used, &resets)}
}

func cacheEntry(t *testing.T, uuid string) (usage.Entry, bool) {
	t.Helper()
	c, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	return c.Get(uuid)
}

// engineFor builds an Engine on a frozen clock with the jitter pinned to its
// midpoint, so a deadline is arithmetic rather than a range.
func engineFor(t *testing.T, token func(context.Context, string) (string, error),
	fetch func(context.Context, string) (*usage.Snapshot, error)) *Engine {
	t.Helper()
	e := NewEngine()
	e.Now = func() time.Time { return tickEpoch }
	e.Rand = func() float64 { return 0.5 }
	e.AccessToken = token
	e.FetchUsage = fetch
	// Both are nilled rather than stubbed, and NewEngine wires both to the
	// real endpoints. Nil is the refusing default for each — a stale
	// credential is not installed, an unnameable login is not overwritten — so
	// a test that wants either behaviour asks for it by name, and one that does
	// not cannot reach the network by forgetting to.
	e.Freshen = nil
	e.ResolveOwner = nil
	return e
}

func tokensAreFine(_ context.Context, uuid string) (string, error) { return "AT-" + uuid, nil }

// tick runs one tick and waits for the fleet it dispatched, so an assertion
// about the cache is not racing the poll that fills it.
func tick(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	e.Wait()
}

func TestATickPollsAnAccountAndCachesTheReading(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(42), nil
	})
	tick(t, e)

	got, ok := cacheEntry(t, "u-1")
	if !ok {
		t.Fatal("the tick cached no reading")
	}
	if pct, ok := got.Snapshot.FiveHour.Percent(); !ok || pct != 42 {
		t.Fatalf("cached utilization = %v (%v), want 42", pct, ok)
	}
	if !got.FetchedAt.Equal(tickEpoch) {
		t.Fatalf("FetchedAt = %s, want the engine's clock %s", got.FetchedAt, tickEpoch)
	}
	// The active account, idle, with no previous sample: ActiveMaxInterval.
	if want := tickEpoch.Add(pollpolicy.ActiveMaxInterval); !got.NextPollAt.Equal(want) {
		t.Fatalf("NextPollAt = %s, want %s", got.NextPollAt, want)
	}
}

// serveTTL is enforced on the fetch path as well as the read path. A reading
// younger than it must not cost a request against a budget that only recovers
// as old requests age out.
func TestATickDoesNotRefetchAFreshReading(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	var polls int
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		polls++
		return snapshotWith(42), nil
	})
	tick(t, e)
	tick(t, e)
	if polls != 1 {
		t.Fatalf("polls = %d, want 1 — the second tick re-fetched a fresh reading", polls)
	}
}

// Never hold a Claude Code lock across a network call, and the daemon is where
// that rule will actually be broken: the poller and the swap executor share
// this process. A fetch that ran with Claude Code's refresh locks held stalls
// Claude Code's own token rotation for the length of an HTTP round trip.
//
// The assertion is made from INSIDE the request handler, which is the only
// place that can observe what is held while the call is in flight.
func TestNoClaudeCodeLockIsHeldAcrossAUsageFetch(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	var lockErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		held, err := cclock.AcquireCredentials(500 * time.Millisecond)
		if err != nil {
			lockErr = err
		} else {
			lockErr = held.Release()
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":42,"resets_at":null}}`))
	}))
	defer srv.Close()

	client := usage.NewClient()
	client.BaseURL = srv.URL
	e := engineFor(t, tokensAreFine, client.FetchUsage)
	tick(t, e)

	if lockErr != nil {
		t.Fatalf("a Claude Code credential lock was held across the fetch: %v", lockErr)
	}
	if _, ok := cacheEntry(t, "u-1"); !ok {
		t.Fatal("the fetch did not reach the cache; the assertion above proved nothing")
	}
}

// The whole point of the daemon: a reading arrives, the engine ranks on it, and
// the swap happens with nobody watching.
func TestATickGoesFromReadingToSwap(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The token names the account, which is how one server answers for two.
		used := 95.0
		if auth := r.Header.Get("Authorization"); auth == "Bearer AT-u-2" {
			used = 5
		}
		fmt.Fprintf(w, `{"five_hour":{"utilization":%v,"resets_at":null}}`, used)
	}))
	defer srv.Close()

	client := usage.NewClient()
	client.BaseURL = srv.URL
	e := engineFor(t, tokensAreFine, client.FetchUsage)

	// The first tick has nothing to rank on: it polls, and standing still is
	// the correct answer while the cache is empty.
	tick(t, e)
	if got := liveUUID(t); got != "u-1" {
		t.Fatalf("live = %q after the first tick, want u-1 — it switched on no evidence", got)
	}
	// The second sees both readings.
	tick(t, e)
	if got := liveUUID(t); got != "u-2" {
		t.Fatalf("live = %q, want u-2 — the account with room", got)
	}
	if at, to := lastSwitchOnDisk(t); to != "u-2" || at.IsZero() {
		t.Fatalf("cooldown stamp = (%v, %q), want one naming u-2", at, to)
	}
}

func lastSwitchOnDisk(t *testing.T) (time.Time, string) {
	t.Helper()
	st, err := strategy.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return st.LastSwitch()
}

// Only a REJECTED refresh token says anything about the account. Quarantining
// on a transport failure takes the whole fleet out the first time a laptop
// sleeps.
func TestOnlyARejectedCredentialQuarantinesAnAccount(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		quarantine bool
	}{
		{
			"the endpoint could not be reached",
			fmt.Errorf("%w: %w", tokens.ErrUnavailable, &oauth.TokenError{Kind: oauth.TokenErrorTransport}),
			false,
		},
		{
			"the refresh token was refused",
			fmt.Errorf("%w: %w", tokens.ErrRejected, &oauth.TokenError{Kind: oauth.TokenErrorInvalidCode, Status: 401}),
			true,
		},
		{"the live login's token has expired", tokens.ErrLiveTokenStale, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateEngine(t)
			seedAccount(t, "u-1", "org-1")
			liveAs(t, "u-1")

			e := engineFor(t, func(context.Context, string) (string, error) { return "", c.err },
				func(context.Context, string) (*usage.Snapshot, error) {
					t.Error("the usage endpoint was called with no token")
					return nil, nil
				})
			tick(t, e)

			st, err := strategy.LoadState()
			if err != nil {
				t.Fatal(err)
			}
			if _, held := st.Quarantined("u-1", tickEpoch); held != c.quarantine {
				t.Fatalf("quarantined = %v, want %v", held, c.quarantine)
			}
		})
	}
}

// An account with no refresh grant behind it is not pollable at all, and
// retrying it every cadence spends nothing but does put a pointless failure in
// the status document on every tick.
func TestAnAccountWithNoOAuthCredentialIsNeverPolled(t *testing.T) {
	isolateEngine(t)
	seedTokenAccount(t, "u-key")

	e := engineFor(t, func(context.Context, string) (string, error) {
		t.Error("a token account was asked for an access token")
		return "", nil
	}, func(context.Context, string) (*usage.Snapshot, error) {
		t.Error("a token account was polled")
		return nil, nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if _, ok := cacheEntry(t, "u-key"); ok {
		t.Fatal("a token account got a cache entry")
	}
	if len(*probes) != 0 {
		t.Fatalf("a token account was probed (%+v); nothing could ever read the window that would wake", *probes)
	}
}

// A 429 is the shared budget saying stop. The backoff has to survive the tick
// that earned it, which means it is persisted with the reading rather than held
// in a map this process owns.
func TestARateLimitBacksOffAndIsPersisted(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "900")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	client := usage.NewClient()
	client.BaseURL = srv.URL

	e := engineFor(t, tokensAreFine, client.FetchUsage)
	tick(t, e)

	got, ok := cacheEntry(t, "u-1")
	if !ok {
		t.Fatal("a rate-limited account has no schedule at all; it will be polled again immediately")
	}
	if got.Snapshot != nil {
		t.Fatal("a 429 wrote a reading; an account that could not be read is UNKNOWN, not empty")
	}
	if want := 900 * time.Second; got.Poll.Interval != want {
		t.Fatalf("Poll.Interval = %s, want the %s the endpoint asked for", got.Poll.Interval, want)
	}
	if !got.Poll.LastRateLimited.Equal(tickEpoch) {
		t.Fatalf("LastRateLimited = %s, want %s", got.Poll.LastRateLimited, tickEpoch)
	}
	if want := tickEpoch.Add(900 * time.Second); !got.NextPollAt.Equal(want) {
		t.Fatalf("NextPollAt = %s, want %s", got.NextPollAt, want)
	}
}

// The budget belongs to the identity. Two accounts in one organization must not
// each poll at the single-account cadence, or the pair spends twice the
// allowance.
func TestAccountsInOneOrganizationShareTheBudget(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-shared")
	seedAccount(t, "u-2", "org-shared")
	seedAccount(t, "u-3", "org-alone")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(10), nil
	})
	tick(t, e)

	shared, _ := cacheEntry(t, "u-1")
	alone, _ := cacheEntry(t, "u-3")
	if want := tickEpoch.Add(2 * pollpolicy.CandidateMaxInterval); !shared.NextPollAt.Equal(want) {
		t.Fatalf("a shared account's NextPollAt = %s, want %s", shared.NextPollAt, want)
	}
	if want := tickEpoch.Add(pollpolicy.CandidateMaxInterval); !alone.NextPollAt.Equal(want) {
		t.Fatalf("a sole account's NextPollAt = %s, want %s", alone.NextPollAt, want)
	}
}

// The authority rule, written out in daemon.Status's "Which file is
// authoritative" comment: quota lives in usage.json and nothing else records
// it, so `list` and `status --json` cannot disagree about a number by reading
// different files.
func TestTheSnapshotCarriesEngineStateAndNoQuota(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(42), nil
	})
	tick(t, e)

	s := e.Snapshot()
	if s.ActiveUUID != "u-1" {
		t.Fatalf("ActiveUUID = %q, want u-1", s.ActiveUUID)
	}
	if len(s.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want one", s.Accounts)
	}
	a := s.Accounts[0]
	if a.State != StateActive {
		t.Fatalf("state = %q, want %q", a.State, StateActive)
	}
	if !a.LastPollAt.Equal(tickEpoch) || a.NextPollAt.IsZero() {
		t.Fatalf("poll times = (%s, %s), want both recorded", a.LastPollAt, a.NextPollAt)
	}
	if a.LastPollError != "" {
		t.Fatalf("LastPollError = %q, want empty", a.LastPollError)
	}
	// The document must not be a second home for quota. Marshalling it and
	// looking for the number is the only check that survives a field being
	// added later.
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"utilization", "headroom", "resetsAt", "percent"} {
		if containsFold(string(raw), banned) {
			t.Fatalf("status carries %q: %s", banned, raw)
		}
	}
}

// A tick must not wait on the network. daemon.Loop waits for the body to
// return, so a tick that awaited a poll would stop publishing status and stop
// executing switches for as long as the endpoint took to answer.
func TestATickDoesNotWaitForItsPollers(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")

	release := make(chan struct{})
	e := engineFor(t, tokensAreFine, func(ctx context.Context, _ string) (*usage.Snapshot, error) {
		<-release
		return snapshotWith(10), nil
	})

	done := make(chan error, 1)
	go func() { done <- e.Tick(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the tick blocked on its poller")
	}
	close(release)
	e.Wait()
}

// The last-good-config rule: a broken hand-edit leaves the engine on the last
// config that PARSED, not on the built-in defaults. A daemon that silently
// reverted a tuned threshold to stock would keep switching, on the wrong
// numbers, saying nothing.
func TestABrokenConfigEditLeavesTheEngineOnTheLastGoodOne(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	writeConfig(t, "strategy = \"consume-first\"\n")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(10), nil
	})
	tick(t, e)
	if got := e.Config().Strategy; got != strategy.StrategyConsumeFirst {
		t.Fatalf("strategy = %v, want the file's", got)
	}

	writeConfig(t, "strategy = = nonsense\n")
	tick(t, e)
	if got := e.Config().Strategy; got != strategy.StrategyConsumeFirst {
		t.Fatalf("strategy = %v, want the last config that parsed", got)
	}
	if e.ConfigError() == nil {
		t.Fatal("the broken edit was not reported at all")
	}
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	root := os.Getenv("CCDAD_HOME")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexFold(s, sub) >= 0
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	for i := range a {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// The movement baseline is persisted for the same reason the backoff is. A
// restarted daemon that saw no previous sample would find nothing moving and
// poll everything at its slowest cadence; one that treated the absence as
// movement would poll everything at its fastest. Neither is what the previous
// process knew.
func TestTheMovementBaselineSurvivesARestart(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")

	reading := 10.0
	fetch := func(context.Context, string) (*usage.Snapshot, error) { return snapshotWith(reading), nil }
	first := engineFor(t, tokensAreFine, fetch)
	tick(t, first)

	// A different process, with none of the first one's memory.
	later := tickEpoch.Add(pollpolicy.CandidateMaxInterval + time.Minute)
	second := engineFor(t, tokensAreFine, fetch)
	second.Now = func() time.Time { return later }
	reading = 30
	tick(t, second)

	got, ok := cacheEntry(t, "u-1")
	if !ok {
		t.Fatal("no reading was cached")
	}
	if want := later.Add(pollpolicy.MinInterval); !got.NextPollAt.Equal(want) {
		t.Fatalf("NextPollAt = %s, want %s — the 20-point move was invisible to the new process",
			got.NextPollAt, want)
	}
}

// A tick must not start a second poll for an account whose first one is still
// running: it spends the identity's budget twice for one reading, and races two
// writers into the same cache row.
func TestATickDoesNotStartASecondPollForTheSameAccount(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")

	release := make(chan struct{})
	var polls atomicCounter
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		polls.inc()
		<-release
		return snapshotWith(10), nil
	})

	for i := 0; i < 3; i++ {
		if err := e.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	e.Wait()
	if got := polls.get(); got != 1 {
		t.Fatalf("polls = %d, want 1 across three ticks", got)
	}
}

type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) inc() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *atomicCounter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// The engine exists only if the daemon process actually runs it. These two pin
// the wiring, because every test above drives the Engine directly and would
// stay green with nothing connected at all.
func TestEngineOptionsWireTheEngineIntoTheProcess(t *testing.T) {
	o := EngineOptions()
	if o.Tick == nil || o.Snapshot == nil {
		t.Fatal("EngineOptions left the tick body or the snapshot unwired")
	}
	if o.Drain == nil {
		t.Fatal("EngineOptions left Drain unwired; a poll could land after the final status")
	}
	if o.Attach == nil {
		t.Fatal("EngineOptions left Attach unwired; the engine would decide silently")
	}
}

// Drain runs after the loop has stopped and before Run returns, so a poll still
// in flight cannot write into the cache after the final status document claimed
// the daemon had stopped.
func TestRunDrainsTheFleetOnTheWayOut(t *testing.T) {
	isolateEngine(t)

	var ticked, drained bool
	ctx, cancel := context.WithCancel(context.Background())
	o := Options{
		Interval: time.Millisecond,
		Tick: func(context.Context) error {
			ticked = true
			cancel()
			return nil
		},
		Drain: func() { drained = true },
	}
	if err := Run(ctx, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ticked {
		t.Fatal("the loop never ran the body")
	}
	if !drained {
		t.Fatal("Run returned without draining")
	}
}

// Attach hands the injected body the daemon's own log, so what the engine
// decided is recoverable with `ccdad daemon logs` rather than lost.
func TestRunHandsTheEngineTheDaemonLog(t *testing.T) {
	isolateEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	var log func(string, ...any)
	o := Options{
		Interval: time.Millisecond,
		Attach:   func(l *Logger) { log = l.Printf },
		Tick: func(context.Context) error {
			if log != nil {
				log("the engine spoke")
			}
			cancel()
			return nil
		},
	}
	if err := Run(ctx, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(mustPath(LogPath()))
	if err != nil {
		t.Fatal(err)
	}
	if !containsFold(string(raw), "the engine spoke") {
		t.Fatalf("daemon.log does not carry what the engine logged:\n%s", raw)
	}
}

// windowsUsed builds a reading with both floors a threshold table can move
// independently. snapshotWith sets the five-hour window alone, so it cannot
// express the account this file has to get right: fine on one axis and spent on
// the other.
func windowsUsed(fiveHour, sevenDay float64) *usage.Snapshot {
	fiveHourResets := tickEpoch.Add(time.Hour)
	weeklyResets := tickEpoch.Add(72 * time.Hour)
	return &usage.Snapshot{
		FiveHour: usage.NewWindow(&fiveHour, &fiveHourResets),
		SevenDay: usage.NewWindow(&sevenDay, &weeklyResets),
	}
}

func stateOf(t *testing.T, s Status, uuid string) AccountState {
	t.Helper()
	for _, a := range s.Accounts {
		if a.UUID == uuid {
			return a.State
		}
	}
	t.Fatalf("no account %q in the published status: %+v", uuid, s.Accounts)
	return ""
}

// An account can be spent by its WEEKLY floor while its five-hour window is
// nowhere near its own. The daemon carried its own copy of the over-threshold
// rule, and that copy knew one number, so it published this account as a healthy
// candidate while the engine ranked it as spent.
//
// End to end through the published document, which is the half a direct call to
// accountState cannot reach: what a dashboard reads is the status the tick
// publishes, not the helper it publishes from.
func TestAnAccountOverItsWeeklyFloorIsPublishedAsExhausted(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	writeConfig(t, "threshold = 80\n\n[window_threshold]\nfive_hour = 85\nseven_day = 60\n")

	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-2" {
			// Seventy-five points of five-hour room against a floor of 85, and
			// ten points PAST a weekly floor of 60.
			return windowsUsed(10, 70), nil
		}
		return windowsUsed(5, 5), nil
	})
	// The first tick dispatches the polls and publishes before either lands, so
	// a state that comes off the cache is the tick after.
	tick(t, e)
	tick(t, e)

	snap := e.Snapshot()
	if got := stateOf(t, snap, "u-2"); got != StateExhausted {
		t.Fatalf("u-2 = %q, want %q — seven_day is 70%% used against a floor of 60", got, StateExhausted)
	}
	if got := stateOf(t, snap, "u-1"); got != StateActive {
		t.Fatalf("u-1 = %q, want %q — it is the live login and it has room", got, StateActive)
	}
}

// The daemon publishes StateExhausted and the engine decides what is spent, and
// those were two pieces of arithmetic in two packages. This is the gate on there
// being one: over a table of readings, the published state and the ranking's own
// verdict have to be the same answer.
func TestTheDaemonAndTheEngineCannotDisagreeAboutSpent(t *testing.T) {
	isolateEngine(t)
	cfg := config.Defaults()
	cfg.Threshold = 80
	cfg.WindowThreshold = map[usage.WindowName]float64{
		usage.WindowFiveHour: 85,
		usage.WindowSevenDay: 60,
	}

	cases := []struct {
		name               string
		fiveHour, sevenDay float64
	}{
		{"neither floor reached", 10, 10},
		{"over the weekly floor only", 10, 70},
		{"over the five-hour floor only", 90, 10},
		{"over both", 90, 70},
		{"on the weekly floor exactly", 10, 60},
		{"a point past the weekly floor", 10, 61},
		{"past the fallback and under both floors of its own", 82, 55},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := windowsUsed(tc.fiveHour, tc.sevenDay)
			cache, err := usage.LoadCache()
			if err != nil {
				t.Fatal(err)
			}
			cache.Put("u-1", usage.Entry{Snapshot: snap, FetchedAt: tickEpoch})

			// activeUUID is empty so the active case cannot answer ahead of the
			// verdict under test.
			state := accountState(store.Account{UUID: "u-1"}, cache, false, "", configuredThresholds(cfg))

			// One candidate, so AllOverThreshold is exactly "this account is
			// known and spent" — the engine's own answer, reached without
			// touching the daemon's.
			res := strategy.Rank([]strategy.Candidate{{
				UUID: "u-1", Kind: identity.KindSubscription, Usage: snap,
			}}, cfg.RankOptions(tickEpoch))

			if got := state == StateExhausted; got != res.AllOverThreshold {
				t.Fatalf("the daemon says exhausted=%v (state %q) and the engine says spent=%v",
					got, state, res.AllOverThreshold)
			}
		})
	}
}

// The same disagreement TestTheDaemonAndTheEngineCannotDisagreeAboutSpent
// closes for the configured threshold, reopened under hover: accountState()
// used to read cfg.Thresholds() directly, which hover never touches, so the
// published engine.state answered against the number hover had already
// replaced.
//
// Two usable accounts (both carry a reading, neither disabled, quarantined or
// api-key), so hover's share is 100/2 = 50. u-1's five_hour window is 15
// minutes into 5 hours — ExpectedPct 5 — so hover's threshold is 5+50 = 55 and
// 60% used is spent (slack -5). The RAW config threshold is 80, under which
// the same 60% reads as healthy (slack +20) — the gap this test is closing.
func TestAccountStateUnderHoverMatchesTheRankingHoverJustRan(t *testing.T) {
	isolateEngine(t)
	writeConfig(t, "hover = true\n")
	seedAccount(t, "u-1", "org-shared")
	seedAccount(t, "u-2", "org-shared")

	u1Pct := 60.0
	u1Reset := tickEpoch.Add(4*time.Hour + 45*time.Minute)
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:  &usage.Snapshot{FiveHour: usage.NewWindow(&u1Pct, &u1Reset)},
		FetchedAt: tickEpoch,
	})
	u2Pct := 10.0
	u2Reset := tickEpoch.Add(time.Hour)
	seedEntry(t, "u-2", usage.Entry{
		Snapshot:  &usage.Snapshot{FiveHour: usage.NewWindow(&u2Pct, &u2Reset)},
		FetchedAt: tickEpoch,
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		t.Fatal("both readings are already fresh; a tick that fetches is not measuring the cached ones this test set up")
		return nil, nil
	})
	tick(t, e)

	var got AccountState
	var found bool
	for _, row := range e.Snapshot().Accounts {
		if row.UUID == "u-1" {
			got, found = row.State, true
		}
	}
	if !found {
		t.Fatal("u-1 has no published row")
	}
	if got != StateExhausted {
		t.Errorf("engine.state for u-1 = %q, want %q — hover's own ranking pass already excludes it "+
			"(60%% against hover's derived threshold of 55); the raw config threshold of 80 would call "+
			"it healthy, which is the disagreement the daemon must not publish", got, StateExhausted)
	}
}

// commit's own consumer of the same table, mirroring
// TestThePollCadenceReadsTheBindingWindowsOwnThreshold's shape: the same
// reading, committed once against the raw configured bundle and once against
// the table hover would have derived for it, must schedule two different
// polls. Equal schedules would mean commit() ignores the resolver it is
// handed and always measures the same number.
//
// u-1 is ACTIVE on purpose: exhausted is checked ahead of the active branch in
// pollpolicy's own cadence rule, but CandidateMaxInterval and
// ExhaustedInterval happen to both be 600s — an inactive fixture would pass
// whether or not the fix worked, for a coincidence that has nothing to do
// with hover. Active splits them: ActiveMaxInterval (300s, healthy) against
// ExhaustedInterval (600s, spent).
//
// Going through commit() directly, not a whole Tick(), is deliberate: a tick
// also runs switcher.Execute, which could move the live account out from
// under u-1 depending on which one hover judges healthiest, and this test is
// about commit()'s resolver, not about who gets switched to.
func TestThePollCadenceMovesWhenHoverMovesAnAccountToSpent(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}
	u1Pct := 60.0
	u1Reset := tickEpoch.Add(4*time.Hour + 45*time.Minute)
	snap := &usage.Snapshot{FiveHour: usage.NewWindow(&u1Pct, &u1Reset)}

	// Two usable accounts is the fixture accountState's hover test above
	// already reasons through: ExpectedPct 5, share 100/2 = 50, hover's
	// threshold 55 against 60% used is spent (slack -5); the raw config
	// threshold of 80 is healthy (slack +20).
	raw := configuredThresholds(config.Config{Threshold: 80})
	hoverDerived := func(string) strategy.Thresholds {
		return strategy.Thresholds{Default: 80, PerWindow: map[usage.WindowName]float64{usage.WindowFiveHour: 55}}
	}

	nextPollAfter := func(thresholds func(string) strategy.Thresholds) time.Time {
		t.Helper()
		e := NewEngine()
		e.Rand = midJitter
		e.commit(a, snap, tickEpoch, []string{a.UUID}, thresholds, true, nil)
		entry, ok := cacheEntry(t, a.UUID)
		if !ok {
			t.Fatal("commit() wrote no cache entry")
		}
		return entry.NextPollAt
	}

	withRaw := nextPollAfter(raw)
	withHover := nextPollAfter(hoverDerived)

	if withRaw.Equal(withHover) {
		t.Errorf("NextPollAt is %s for both the raw table and hover's, so commit() measured the "+
			"same 60%% against the same threshold both times — it never asked the resolver it was "+
			"handed", withRaw)
	}
	if want := tickEpoch.Add(pollpolicy.ActiveMaxInterval); !withRaw.Equal(want) {
		t.Errorf("against the raw config threshold of 80, NextPollAt = %s, want %s "+
			"(ActiveMaxInterval) — 60%% used is healthy", withRaw, want)
	}
	if want := tickEpoch.Add(pollpolicy.ExhaustedInterval); !withHover.Equal(want) {
		t.Errorf("against hover's derived threshold of 55, NextPollAt = %s, want %s "+
			"(ExhaustedInterval) — 60%% used is spent", withHover, want)
	}
}

// seedEntry puts one row into the usage cache directly, so a test can describe a
// reading the daemon has already taken without pretending to take it.
func seedEntry(t *testing.T, uuid string, e usage.Entry) {
	t.Helper()
	if err := usage.WithCache(cacheTimeout, func(c *usage.Cache) error {
		c.Put(uuid, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The urgent band is "within 15 points of the threshold", and the threshold it
// means is the BINDING window's own. The cadence used to be handed the global
// fallback instead, which is a different number the moment a window carries one:
// an account whose weekly floor is 60 against a fallback of 80 had the band
// placed 20 points too high, so it was polled at the ordinary cadence through
// the exact span the band exists to cover.
//
// Nobody would have noticed. This decides a poll interval, not a switch, and the
// published state is right either way — which is why the assertion is on the
// schedule rather than on anything a user reads.
func TestThePollCadenceReadsTheBindingWindowsOwnThreshold(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}

	// ACTIVE and MOVING, because the urgent rule is an AND of both halves plus
	// the band: without the movement the account gets ActiveMaxInterval whatever
	// the threshold is, and the two answers would be identical for opposite
	// reasons.
	nextPollAfter := func(cfg config.Config) time.Time {
		t.Helper()
		seedEntry(t, a.UUID, usage.Entry{
			Snapshot:  windowsUsed(10, 45),
			FetchedAt: tickEpoch.Add(-time.Hour),
			Poll:      usage.PollState{LastBindingPct: 45, HasLastBinding: true},
		})
		e := NewEngine()
		e.Rand = midJitter
		e.commit(a, windowsUsed(10, 50), tickEpoch, []string{a.UUID}, configuredThresholds(cfg), true, nil)
		entry, ok := cacheEntry(t, a.UUID)
		if !ok {
			t.Fatal("commit() wrote no cache entry")
		}
		return entry.NextPollAt
	}

	// seven_day is 50% used, five_hour 10%. Under the table below seven_day has
	// 10 points of slack against five_hour's 70, so it binds — and 50 is 5
	// points inside a band that reaches down to 45.
	tuned := nextPollAfter(config.Config{
		Threshold:       80,
		WindowThreshold: map[usage.WindowName]float64{usage.WindowSevenDay: 60},
	})
	if want := tickEpoch.Add(pollpolicy.UrgentInterval); !tuned.Equal(want) {
		t.Errorf("the next poll is %v, want %v — 50%% used is 10 points from a floor of 60, "+
			"which is inside the urgent band; measured against the fallback of 80 it is 30 points out",
			tuned, want)
	}

	// The fallback still reaches every window that has no entry of its own, so
	// the same reading with no table is genuinely outside the band.
	bare := nextPollAfter(config.Config{Threshold: 80})
	if want := tickEpoch.Add(pollpolicy.MinInterval); !bare.Equal(want) {
		t.Errorf("with no table the next poll is %v, want %v — 50%% used against a threshold "+
			"of 80 is moving but not near it", bare, want)
	}
}

// seedDisabled stores an account that can never be a switch target. It is still
// polled and still draws on its identity's budget: dispatch asks pollable(),
// which is about credentials, and nothing else.
func seedDisabled(t *testing.T, uuid, org string) {
	t.Helper()
	a := seedAccount(t, uuid, org)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDisabled(a.UUID, true); err != nil {
		t.Fatal(err)
	}
}

// The danger band, end to end. Three accounts share one organization, so the
// ordinary rules put the live one on 3 x 60 s; five points from the endpoint's
// refusal that is two thirds of the remaining room spent unread.
func TestTheLiveAccountInTheDangerBandPollsEveryMinuteUnshared(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-shared")
	seedDisabled(t, "u-2", "org-shared")
	seedDisabled(t, "u-3", "org-shared")
	liveAs(t, "u-1")

	var live atomicCounter
	now := tickEpoch
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-1" {
			live.inc()
			return snapshotWith(97), nil
		}
		return snapshotWith(20), nil
	})
	e.Now = func() time.Time { return now }
	tick(t, e)

	got, ok := cacheEntry(t, "u-1")
	if !ok {
		t.Fatal("the tick cached no reading")
	}
	if want := tickEpoch.Add(pollpolicy.UrgentInterval); !got.NextPollAt.Equal(want) {
		t.Fatalf("NextPollAt = %s, want %s — three accounts share this identity and the "+
			"account a session is running against is the one exemption", got.NextPollAt, want)
	}
	if got.ServeTTL != pollpolicy.DangerServeTTL {
		t.Fatalf("ServeTTL = %s, want %s", got.ServeTTL, pollpolicy.DangerServeTTL)
	}

	// And the cadence is real rather than only written down: the second poll
	// lands a minute later, which the flat 180 s serve TTL would have refused.
	now = tickEpoch.Add(pollpolicy.UrgentInterval + time.Second)
	tick(t, e)
	if n := live.get(); n != 2 {
		t.Fatalf("the live account was polled %d times, want 2 — the 60 s cadence was "+
			"clamped by the serve TTL", n)
	}
	if l := liveUUID(t); l != "u-1" {
		t.Fatalf("live = %q, want u-1 — the engine switched, so nothing above was about the band", l)
	}
}

// The other half of the reallocation. While the live account is in the band the
// rest of its identity yields, because the budget is per identity and 60 s on
// one account is most of it.
func TestTheDangerBandStandsTheRestOfTheIdentityDown(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-shared")
	seedDisabled(t, "u-2", "org-shared")
	seedDisabled(t, "u-solo", "org-alone")
	liveAs(t, "u-1")

	// Both alternates already hold a fresh reading, so neither is due and the
	// only writer is the live account's own poll. An account with NO entry is
	// skipped by a stand-down, and on a first tick whether it has one yet is a
	// race between two pollers.
	earned := tickEpoch.Add(2 * pollpolicy.CandidateMaxInterval)
	seedEntry(t, "u-2", usage.Entry{
		Snapshot: snapshotWith(20), FetchedAt: tickEpoch, NextPollAt: earned,
	})
	seedEntry(t, "u-solo", usage.Entry{
		Snapshot: snapshotWith(20), FetchedAt: tickEpoch,
		NextPollAt: tickEpoch.Add(pollpolicy.CandidateMaxInterval),
	})

	now := tickEpoch
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-1" {
			return snapshotWith(97), nil
		}
		return snapshotWith(20), nil
	})
	e.Now = func() time.Time { return now }
	tick(t, e)

	sib, ok := cacheEntry(t, "u-2")
	if !ok {
		t.Fatal("the account sharing the identity has no entry")
	}
	pushed := tickEpoch.Add(pollpolicy.Post429MaxInterval)
	if !sib.StandDownUntil.Equal(pushed) {
		t.Fatalf("StandDownUntil = %s, want %s", sib.StandDownUntil, pushed)
	}
	// The schedule its own poll earned is untouched. A stand-down that overwrote
	// it would overwrite a 429's floor with something shorter.
	if !sib.NextPollAt.Equal(earned) {
		t.Fatalf("NextPollAt = %s, want the %s it came in with", sib.NextPollAt, earned)
	}

	// A second band commit a minute later does NOT push the deadline out again.
	// The band recommits every 60 s, so a renewal on each one would move the
	// deadline away faster than the clock reaches it and the alternate would
	// never be polled at all.
	now = tickEpoch.Add(pollpolicy.UrgentInterval + time.Second)
	tick(t, e)
	sib, _ = cacheEntry(t, "u-2")
	if !sib.StandDownUntil.Equal(pushed) {
		t.Fatalf("StandDownUntil = %s after a second band commit, want the original %s — "+
			"a renewed stand-down never expires", sib.StandDownUntil, pushed)
	}
	// An account on a different identity draws on a different budget and has
	// nothing to yield.
	other, ok := cacheEntry(t, "u-solo")
	if !ok {
		t.Fatal("the account on the other identity has no entry")
	}
	if !other.StandDownUntil.IsZero() {
		t.Fatalf("StandDownUntil = %s on another identity, want none", other.StandDownUntil)
	}
}

// A stand-down is written for the accounts that do not matter right now, and a
// switch changes which one that is. The account Claude Code is logged in as is
// never held by one — it is the only account a session can be cut off on.
func TestTheLiveAccountIsNeverHeldByAStandDown(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-shared")
	seedAccount(t, "u-2", "org-shared")
	liveAs(t, "u-2")

	stood := tickEpoch.Add(pollpolicy.Post429MaxInterval)
	seedEntry(t, "u-1", usage.Entry{
		Snapshot: snapshotWith(50), FetchedAt: tickEpoch.Add(-time.Hour), StandDownUntil: stood,
	})
	seedEntry(t, "u-2", usage.Entry{
		Snapshot: snapshotWith(5), FetchedAt: tickEpoch.Add(-time.Hour), StandDownUntil: stood,
	})

	var one, two atomicCounter
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-1" {
			one.inc()
			return snapshotWith(50), nil
		}
		two.inc()
		return snapshotWith(5), nil
	})
	tick(t, e)

	if n := two.get(); n != 1 {
		t.Fatalf("the live account was polled %d times, want 1 — a stand-down written for "+
			"its predecessor blinded the engine on it", n)
	}
	if n := one.get(); n != 0 {
		t.Fatalf("a stood-down account was polled %d times, want 0", n)
	}
}

// Unknown is never 97. A poll that failed says nothing about the account, and a
// failure that stood the identity down would silence every alternate for half an
// hour on no evidence — at the exact moment the engine needs one to switch to.
func TestAFailedPollNeverStandsTheIdentityDown(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-shared")
	seedDisabled(t, "u-2", "org-shared")
	liveAs(t, "u-1")

	// The live account was inside the band when it was last read, and the poll
	// about to run does not answer. Its alternate's reading is fresh, so nothing
	// but a stand-down could move that one's schedule.
	seedEntry(t, "u-1", usage.Entry{Snapshot: snapshotWith(97), FetchedAt: tickEpoch.Add(-time.Hour)})
	seedEntry(t, "u-2", usage.Entry{Snapshot: snapshotWith(10), FetchedAt: tickEpoch})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return nil, errors.New("the usage endpoint did not answer")
	})
	tick(t, e)

	sib, _ := cacheEntry(t, "u-2")
	if !sib.StandDownUntil.IsZero() {
		t.Fatalf("StandDownUntil = %s, want none — a failed poll stood the identity down", sib.StandDownUntil)
	}
	got, _ := cacheEntry(t, "u-1")
	if got.ServeTTL != pollpolicy.ServeTTL {
		t.Fatalf("ServeTTL = %s, want the ordinary %s", got.ServeTTL, pollpolicy.ServeTTL)
	}
}

// unusedWindow is what the endpoint reports for a window nothing has ever been
// spent against: a utilization it can name and no reset time at all. snapshotWith
// cannot express it — that helper always names a reset — and this is the exact
// condition a probe exists to remove.
func unusedWindow() *usage.Snapshot {
	zero := 0.0
	return &usage.Snapshot{FiveHour: usage.NewWindow(&zero, nil)}
}

// seedLiveHolder puts a live account beside the one a test means to probe,
// holding a reading identical to it.
//
// Without one the engine SWITCHES to the account under test, and then never
// probes it. That is the right behaviour — an account whose window has never
// been used is at 0% of it, which is the most slack there is, so it wins a pool
// with nobody else in it, and the live account gets its reset time from the
// user's own next turn rather than from a probe. It is just not the behaviour
// these tests are about. A holder on an identical reading ties with it, and a tie
// does not clear the hysteresis margin, so nothing moves.
func seedLiveHolder(t *testing.T, uuid string) {
	t.Helper()
	seedAccount(t, uuid, "org-live")
	liveAs(t, uuid)
	seedEntry(t, uuid, usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch})
}

// probeCall is one probe the engine asked for.
type probeCall struct{ uuid, model string }

// stubProbe records what the engine would have spawned, and puts a claude on
// this machine's PATH for the duration.
//
// No mutex: probes are decided in dispatch, which runs on the tick goroutine and
// nowhere else, unlike the polls dispatch hands to goroutines.
func stubProbe(t *testing.T, e *Engine) *[]probeCall {
	t.Helper()
	saved := lookClaude
	t.Cleanup(func() { lookClaude = saved })
	lookClaude = func(string) (string, error) { return filepath.Join(t.TempDir(), "claude"), nil }

	var calls []probeCall
	e.SpawnProbe = func(uuid, model string) error {
		calls = append(calls, probeCall{uuid, model})
		return nil
	}
	return &calls
}

// The feature in one assertion. An account holding a reading whose binding window
// reports no reset time is unrankable — no reset, no pace, no projection — and
// the only thing that changes that is spending against the window.
//
// The second half is the part that is easy to get wrong: the tick must NOT also
// poll. The probe has already spent the inference budget, and a poll now would
// spend the usage budget for a reading the endpoint does not have yet.
func TestTheDaemonProbesAnAccountWhoseBindingWindowHasNoResetTime(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		t.Error("the tick polled the account it had just probed; the reading is not there yet")
		return nil, nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 1 || (*probes)[0].uuid != "u-1" {
		t.Fatalf("probes = %+v, want exactly one for u-1", *probes)
	}
	got, ok := cacheEntry(t, "u-1")
	if !ok {
		t.Fatal("the probe left no cache row, so it would be queued again on the next tick")
	}
	if !got.Probe.LastAttemptAt.Equal(tickEpoch) {
		t.Fatalf("Probe.LastAttemptAt = %s, want the engine's clock %s", got.Probe.LastAttemptAt, tickEpoch)
	}
	if want := tickEpoch.Add(usage.ProbePollDelay); !got.NextPollAt.Equal(want) {
		t.Fatalf("NextPollAt = %s, want %s — a poll now spends the usage budget for a "+
			"reading the endpoint does not have yet", got.NextPollAt, want)
	}
	if got.Snapshot == nil {
		t.Fatal("the probe erased the reading that told it to probe")
	}
}

// The condition that stops the probe recurring once it has worked: a window that
// reports a reset time has nothing left to wake, and probing it would spend quota
// on every cadence forever.
func TestTheDaemonDoesNotProbeAWindowThatAlreadyReportsAReset(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedEntry(t, "u-1", usage.Entry{Snapshot: snapshotWith(30), FetchedAt: tickEpoch.Add(-10 * time.Minute)})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(30), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 0 {
		t.Fatalf("probes = %+v, want none — that window already reports a reset", *probes)
	}
}

// The production shape this guards: a five-hour window nothing has ever spent
// against, sitting beside a weekly window that already binds tighter AND
// already has its own reset. HeadroomOf's Binding names the weekly window
// here, but the five-hour window's missing reset is no less missing for it —
// hover status marks that row probe-worthy on exactly this reasoning, and the
// engine has to agree with its own status output.
func TestTheDaemonProbesAnUnspentWindowEvenWhenADifferentOneBindsTighter(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	unspent, spent := 0.0, 70.0
	weeklyResets := tickEpoch.Add(72 * time.Hour)
	seedEntry(t, "u-1", usage.Entry{
		FetchedAt: tickEpoch.Add(-10 * time.Minute),
		Snapshot: &usage.Snapshot{
			FiveHour: usage.NewWindow(&unspent, nil),
			SevenDay: usage.NewWindow(&spent, &weeklyResets),
		},
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		t.Error("the tick polled the account it had just probed; the reading is not there yet")
		return nil, nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 1 || (*probes)[0].uuid != "u-1" {
		t.Fatalf("probes = %+v, want exactly one for u-1 — its five-hour window has never been "+
			"spent and carries no reset, even though the weekly window binds tighter and already "+
			"has one", *probes)
	}
}

// A probe that fails must not be retried at the tick loop's cadence: it spends
// the account's own quota every time. The stamp the engine writes before the
// spawn is what holds the gate even for a spawn that never started.
func TestTheDaemonDoesNotProbeAnAccountAgainForSixHours(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)})

	now := tickEpoch
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return unusedWindow(), nil
	})
	e.Now = func() time.Time { return now }
	probes := stubProbe(t, e)

	tick(t, e)
	if len(*probes) != 1 {
		t.Fatalf("probes = %+v after the first tick, want exactly one", *probes)
	}
	// An hour on, with the reading still carrying no reset: still held.
	now = tickEpoch.Add(time.Hour)
	tick(t, e)
	if len(*probes) != 1 {
		t.Fatalf("probes = %+v an hour later, want the first one only", *probes)
	}
	// Past the gate, and open again on its own — a probe that failed once must
	// not be abandoned forever either.
	now = tickEpoch.Add(usage.ProbeRetryAfter + time.Minute)
	tick(t, e)
	if len(*probes) != 2 {
		t.Fatalf("probes = %+v after the gate lifted, want two", *probes)
	}
}

// The switch that turns the whole thing off, and the account state that opts out
// of it without any configuration at all.
func TestTheDaemonProbesNothingItWasToldNotTo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		cfg   string
	}{
		{"probe_unknown = false", func(t *testing.T) { seedAccount(t, "u-1", "org-1") }, "probe_unknown = false\n"},
		{"a disabled account", func(t *testing.T) { seedDisabled(t, "u-1", "org-1") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEngine(t)
			tc.setup(t)
			// The holder is what makes "no probe" mean the switch under test
			// rather than the account simply having become live.
			seedLiveHolder(t, "u-live")
			if tc.cfg != "" {
				writeConfig(t, tc.cfg)
			}
			seedEntry(t, "u-1", usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)})

			e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
				return unusedWindow(), nil
			})
			probes := stubProbe(t, e)
			tick(t, e)

			if len(*probes) != 0 {
				t.Fatalf("probes = %+v, want none", *probes)
			}
		})
	}
}

// The account Claude Code is logged in as is the one something else is already
// spending against, so probing it is the one probe that duplicates work — and the
// one that can revoke the refresh token the live session is using, because the
// probe's own Claude Code holds the same login in a different credential home and
// the server rotates on refresh. Nothing happens instead: the window gets its
// reset from the user's own next turn.
func TestTheDaemonNeverProbesTheAccountThatIsLive(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	for _, uuid := range []string{"u-1", "u-2"} {
		seedEntry(t, uuid, usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)})
	}

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return unusedWindow(), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	for _, c := range *probes {
		if c.uuid == "u-1" {
			t.Fatalf("probed the live account: %+v", *probes)
		}
	}
	if len(*probes) != 1 || (*probes)[0].uuid != "u-2" {
		t.Fatalf("probes = %+v, want exactly one for the account that is not live", *probes)
	}
}

// A per-model weekly window is only woken by a turn spent against that model, so
// the family the engine derives from the binding window is the whole difference
// between a probe that works and one that spends quota for nothing.
func TestTheDaemonAsksForTheModelTheBindingWindowNeeds(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	// Least slack is what binds, and a window nothing has been spent against is
	// at 0% — the most slack there is at one threshold for every window. A floor
	// of its own is what makes an unused window bind, which is also the only
	// configuration in which this account is worth probing at all.
	writeConfig(t, "threshold = 80\n\n[window_threshold]\nseven_day_opus = 10\n")
	used, unspent := 5.0, 0.0
	resets := tickEpoch.Add(time.Hour)
	seedEntry(t, "u-1", usage.Entry{
		FetchedAt: tickEpoch.Add(-10 * time.Minute),
		Snapshot: &usage.Snapshot{
			FiveHour:     usage.NewWindow(&used, &resets),
			SevenDayOpus: usage.NewWindow(&unspent, nil),
		},
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		t.Error("the tick polled the account it had just probed")
		return nil, nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 1 {
		t.Fatalf("probes = %+v, want exactly one", *probes)
	}
	if (*probes)[0].model != "opus" {
		t.Fatalf("probe model = %q, want \"opus\" — the weekly Opus window is the one with no reset",
			(*probes)[0].model)
	}
}

// A machine with no Claude Code stays that way, and this loop runs about once a
// second. One line, once, saying what the daemon cannot do and how to stop it
// looking — and no attempt recorded, so the account is probed the moment claude
// appears rather than six hours later.
//
// The account is still POLLED on a machine that cannot probe, which is what the
// second tick below is standing on: a probe the daemon could not start replaces
// nothing, and skipping the poll too would leave an account that later gets a
// reset time of its own unread forever.
func TestNoClaudeOnPATHIsSaidOnceAndSpendsNoBudget(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)})

	var lines []string
	var polls atomicCounter
	now := tickEpoch
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-1" {
			polls.inc()
		}
		return unusedWindow(), nil
	})
	e.Now = func() time.Time { return now }
	e.Log = func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	e.SpawnProbe = func(string, string) error {
		t.Error("a probe was started on a machine with no claude on it")
		return nil
	}
	// isolateEngine already describes a machine with no claude; stated here
	// because this test is ABOUT that machine rather than merely sandboxed on it.

	tick(t, e)
	// Far enough on that the account is due again, so the second tick genuinely
	// reaches the decision rather than being turned away at the freshness gate.
	now = tickEpoch.Add(pollpolicy.CandidateMaxInterval + time.Minute)
	tick(t, e)

	said := 0
	for _, l := range lines {
		if containsFold(l, "not probing") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("the daemon said it could not probe %d times over two ticks, want 1:\n%v", said, lines)
	}
	got, _ := cacheEntry(t, "u-1")
	if !got.Probe.LastAttemptAt.IsZero() {
		t.Fatalf("a probe that never ran consumed the six-hour budget (%s)", got.Probe.LastAttemptAt)
	}
	// The poll is what a probe replaces, so a probe that never started replaces
	// nothing. Skipping it as well would be the worse bug: the reading is the
	// only thing that says the window still has no reset, so an account never
	// polled again could never be found to have got one.
	if n := polls.get(); n == 0 {
		t.Fatal("the account was neither probed nor polled, so nothing will ever read it again")
	}
}

// The user's quota is being spent on their behalf, so it is said out loud —
// once, because this loop runs at 1 Hz and a line it repeats is a line nobody
// reads.
func TestTheFirstAutomaticProbeSaysItSpendsTheUsersQuota(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	seedLiveHolder(t, "u-live")
	for _, uuid := range []string{"u-1", "u-2"} {
		seedEntry(t, uuid, usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)})
	}

	var lines []string
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return unusedWindow(), nil
	})
	e.Log = func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 2 {
		t.Fatalf("probes = %+v, want two", *probes)
	}
	said := 0
	for _, l := range lines {
		if containsFold(l, "SPENDS that account's own quota") {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("the quota line appeared %d times for two probes, want 1:\n%v", said, lines)
	}
}

// A stand-down is a yield, not a stop. The band recommits every 60 s for as long
// as the live account stays inside it, so a deadline renewed on every one of
// those would move away faster than the clock reaches it and the alternate would
// go permanently unread — the accounts standing down being exactly the ones the
// engine will want to switch TO, that is the band blinding the engine to the
// escape it exists to buy time for.
func TestAStoodDownAccountIsStillPolledAboutTwiceAnHour(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-shared")
	seedDisabled(t, "u-2", "org-shared")
	liveAs(t, "u-1")

	var alt atomicCounter
	now := tickEpoch
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-1" {
			return snapshotWith(97), nil
		}
		alt.inc()
		return snapshotWith(20), nil
	})
	e.Now = func() time.Time { return now }

	// Two hours at the tick loop's own cadence, coarsened to 30 s so the test is
	// arithmetic rather than four thousand ticks.
	const span = 2 * time.Hour
	for at := time.Duration(0); at <= span; at += 30 * time.Second {
		now = tickEpoch.Add(at)
		tick(t, e)
	}

	// Its own cadence is 2 x 600 s and the stand-down is 1800 s, so it comes back
	// on the stand-down: once at t=0 and then about every half hour.
	if n := alt.get(); n < 3 {
		t.Fatalf("the alternate was polled %d times in two hours, want at least 3 — "+
			"a stand-down that is renewed on every band commit never expires", n)
	}
	sib, _ := cacheEntry(t, "u-2")
	if age := now.Sub(sib.FetchedAt); age > pollpolicy.Post429MaxInterval+pollpolicy.CandidateMaxInterval {
		t.Fatalf("the alternate's reading is %s old at the end of the run — the engine "+
			"cannot rank the account it is being held back for", age)
	}
	if l := liveUUID(t); l != "u-1" {
		t.Fatalf("live = %q, want u-1", l)
	}
}

// A stand-down written while an account was an alternate must stop being
// published as that account's own deadline the moment it becomes live, because
// the dispatcher stops applying it at exactly that moment. A status document
// that says half an hour about an account the daemon will poll in ten minutes is
// the same disagreement, in the other direction, that this document exists to
// prevent.
func TestAStandDownIsNotPublishedForTheAccountThatBecameLive(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-shared")
	// Disabled, so the engine cannot switch to it on its own while it is the
	// alternate — the switch below is the user's, and an account taken out of
	// rotation while ccdad is logged in as it is an ordinary state.
	seedDisabled(t, "u-2", "org-shared")
	liveAs(t, "u-1")

	earned := tickEpoch.Add(pollpolicy.CandidateMaxInterval)
	seedEntry(t, "u-2", usage.Entry{
		Snapshot: snapshotWith(20), FetchedAt: tickEpoch, NextPollAt: earned,
	})

	now := tickEpoch
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-1" {
			return snapshotWith(97), nil
		}
		return snapshotWith(20), nil
	})
	e.Now = func() time.Time { return now }
	tick(t, e)

	sib, _ := cacheEntry(t, "u-2")
	if sib.StandDownUntil.IsZero() {
		t.Fatal("the alternate was never stood down, so this test is not about anything")
	}
	// It is the alternate, so the stand-down IS its deadline.
	if got := nextPollOf(t, e.Snapshot(), "u-2"); !got.Equal(sib.StandDownUntil) {
		t.Fatalf("published nextPollAt = %s for a stood-down alternate, want %s", got, sib.StandDownUntil)
	}

	// Now it is live. The dispatcher stops honouring the stand-down here, and so
	// must the document.
	liveAs(t, "u-2")
	now = tickEpoch.Add(time.Second)
	tick(t, e)
	if got := nextPollOf(t, e.Snapshot(), "u-2"); !got.Equal(earned) {
		t.Fatalf("published nextPollAt = %s for the account that just became live, want the "+
			"%s its own poll earned — the dispatcher will reach it then", got, earned)
	}
}

func nextPollOf(t *testing.T, s Status, uuid string) time.Time {
	t.Helper()
	for _, a := range s.Accounts {
		if a.UUID == uuid {
			return a.NextPollAt
		}
	}
	t.Fatalf("no account %q in the published status", uuid)
	return time.Time{}
}

// A probe and a poll of the same account must not be in flight together. The
// poll was dispatched by an earlier tick and has not committed; when it does it
// writes an ordinary cadence over the one-minute schedule the probe just bought,
// so the poll meant to read what the probe woke lands ten minutes out instead of
// one — on a turn of quota already spent.
func TestAnAccountIsNotProbedWhileItsOwnPollIsStillRunning(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	// Probe-due on every axis except the six-hour gate, which opens between the
	// two ticks below.
	seedEntry(t, "u-1", usage.Entry{
		Snapshot:  unusedWindow(),
		FetchedAt: tickEpoch.Add(-10 * time.Minute),
		Probe:     usage.ProbeState{LastAttemptAt: tickEpoch.Add(-usage.ProbeRetryAfter + time.Minute)},
	})

	release := make(chan struct{})
	now := tickEpoch
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-1" {
			<-release
		}
		return unusedWindow(), nil
	})
	e.Now = func() time.Time { return now }
	probes := stubProbe(t, e)

	// The first tick dispatches a poll that never finishes.
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The second is past the six-hour gate, so nothing but the in-flight poll
	// stands between this account and a probe.
	now = tickEpoch.Add(2 * time.Minute)
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(*probes); n != 0 {
		t.Fatalf("probes = %+v while a poll of the same account was in flight", *probes)
	}
	close(release)
	e.Wait()
}

// A quarantined account is out of rotation, and the only thing that quarantines
// is a refresh token the endpoint has already rejected — which is the very
// credential a probe would seed its Claude Code with. The errand cannot
// authenticate, so it would be sent, and fail, every six hours forever.
func TestTheDaemonNeverProbesAQuarantinedAccount(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)})
	if err := strategy.WithState(stateTimeout, func(st *strategy.State) error {
		st.Quarantine("u-1", tickEpoch, time.Hour, strategy.RefreshDead.String())
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return unusedWindow(), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 0 {
		t.Fatalf("probes = %+v, want none — that account's refresh grant is the one the "+
			"endpoint already rejected", *probes)
	}
}

// "Which account is live could not be worked out" is not "no account is live".
// AttributeFile matches the credentials file by refresh token and the server
// rotates that token on every refresh, so attribution lapses on exactly the
// account a session is running against — and a probe of that one is the probe
// that can revoke the token the session is using.
func TestTheDaemonNeverProbesWhenItCannotTellWhichAccountIsLive(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedEntry(t, "u-1", usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)})
	// A live login whose refresh token matches nothing in the store: the shape a
	// rotation leaves behind. And an OAuth token in the environment, which is
	// what stops the engine from resolving the ambiguity by switching — an
	// unattended swap stands down under an override, so the daemon runs a whole
	// tick without ever learning who is live. That is the state under test.
	body := `{"claudeAiOauth":{"accessToken":"AT-rotated","refreshToken":"RT-u-1-rotated"}}`
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-overriding")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return unusedWindow(), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if l := liveUUID(t); l != "" {
		t.Fatalf("the fixture attributes to %q, so this test is not about an unattributable login", l)
	}
	if len(*probes) != 0 {
		t.Fatalf("probes = %+v while the live account was unknown — one of them may be it", *probes)
	}
}

// The same guard at the decision itself, because the end-to-end fixture above
// has to arrange a machine that cannot resolve the ambiguity by switching, and
// an edit that made the guard depend on the empty uuid rather than on the flag
// would still pass it.
func TestProbeDueRefusesAnUnknownLiveAccountAndNotMerelyAnEmptyOne(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	e.Now = func() time.Time { return tickEpoch }
	a := store.Account{UUID: "u-1"}
	entry := usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)}
	cfg := config.Defaults()

	thresholds := configuredThresholds(cfg)
	if _, want := e.probeDue(a, entry, cfg, thresholds, tickEpoch, "", false, nil); want {
		t.Error("probed while which account is live could not be worked out")
	}
	if _, want := e.probeDue(a, entry, cfg, thresholds, tickEpoch, "", true, nil); !want {
		t.Error("refused to probe on a machine where nothing is live, which is the safe case")
	}
	if _, want := e.probeDue(a, entry, cfg, thresholds, tickEpoch, "u-1", true, nil); want {
		t.Error("probed the live account")
	}
}

// The endpoint answers null for resets_at until something has been spent, so a
// window that is 40% used and still names no reset is not an unused window. In
// practice it is a resets_at this build could not parse, which the snapshot
// parser degrades to "no reset" on purpose — and the next reading carries the
// same unparseable time, so a probe here spends a turn of the user's quota every
// six hours for as long as the drift lasts.
func TestTheDaemonDoesNotProbeAWindowThatHasAlreadyBeenSpentAgainst(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	spent := 40.0
	seedEntry(t, "u-1", usage.Entry{
		FetchedAt: tickEpoch.Add(-10 * time.Minute),
		Snapshot:  &usage.Snapshot{FiveHour: usage.NewWindow(&spent, nil)},
	})

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return unusedWindow(), nil
	})
	probes := stubProbe(t, e)
	tick(t, e)

	if len(*probes) != 0 {
		t.Fatalf("probes = %+v — 40%% of that window has been spent, so the missing reset "+
			"is a reading this build could not parse and no turn of quota will change it", *probes)
	}
}

// The attempt is stamped BEFORE the probe is started, so a probe that fails to
// start still consumes the six-hour budget. Without that order an unstartable
// probe is attempted on every tick, forever.
func TestAProbeThatFailsToStartStillConsumesTheBudget(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedLiveHolder(t, "u-live")
	seedEntry(t, "u-1", usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)})

	var attempts atomicCounter
	now := tickEpoch
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return unusedWindow(), nil
	})
	e.Now = func() time.Time { return now }
	stubProbe(t, e)
	e.SpawnProbe = func(string, string) error {
		attempts.inc()
		return errors.New("fork/exec: resource temporarily unavailable")
	}

	tick(t, e)
	if n := attempts.get(); n != 1 {
		t.Fatalf("the engine tried %d probes on the first tick, want 1", n)
	}
	got, _ := cacheEntry(t, "u-1")
	if !got.Probe.LastAttemptAt.Equal(tickEpoch) {
		t.Fatalf("Probe.LastAttemptAt = %s after a spawn that failed, want %s — an "+
			"unstartable probe would be attempted on every tick forever", got.Probe.LastAttemptAt, tickEpoch)
	}
	now = tickEpoch.Add(usage.ProbeRetryAfter - time.Minute)
	tick(t, e)
	if n := attempts.get(); n != 1 {
		t.Fatalf("the engine tried %d probes inside the six-hour gate, want 1", n)
	}
}

// probeModel hands ModelFamily the DISPLAY half of a scoped name and never the
// whole thing. ModelFamily is a substring match and the full name carries the
// scope KEY, so a deployment whose keys name a model would otherwise have a
// family read out of a string that names no model at all — and the probe would
// spend a turn against a window nobody asked about.
func TestTheProbeModelIsReadFromTheScopeDisplayNameAlone(t *testing.T) {
	for _, tc := range []struct {
		window usage.WindowName
		want   string
	}{
		{usage.WindowFiveHour, ""},
		{usage.WindowSevenDay, ""},
		{usage.WindowSevenDayOAuthApps, ""},
		{usage.WindowSevenDayOpus, "opus"},
		{usage.WindowSevenDaySonnet, "sonnet"},
		{usage.ScopedWindowName(usage.ScopeModel, "Opus 4.5"), "opus"},
		{usage.ScopedWindowName(usage.ScopeModel, "Sonnet 4.5"), "sonnet"},
		{usage.ScopedWindowName(usage.ScopeModel, "Team plan"), ""},
		// A SURFACE scope names no model, and its display name is free text that
		// may contain one.
		{usage.ScopedWindowName(usage.ScopeSurface, "Claude Code"), ""},
		{usage.ScopedWindowName(usage.ScopeSurface, "Opus workbench"), ""},
		// The scope key itself is free text on the wire, and it is the half that
		// must never reach the matcher.
		{usage.WindowName("weekly_scoped:sonnet_tier:Team"), ""},
	} {
		t.Run(string(tc.window), func(t *testing.T) {
			if got := probeModel(tc.window); got != tc.want {
				t.Fatalf("probeModel(%q) = %q, want %q", tc.window, got, tc.want)
			}
		})
	}
}

// The live account never writes a stand-down against itself. usage.Entry.PollAt
// ignores one while the account is live, so nothing happens today — and then a
// switch happens, and the account that was just switched AWAY from is held for
// up to half an hour at the one moment its recovery is worth watching for.
func TestTheLiveAccountDoesNotStandItselfDown(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-shared")
	seedDisabled(t, "u-2", "org-shared")
	liveAs(t, "u-1")
	seedEntry(t, "u-2", usage.Entry{Snapshot: snapshotWith(20), FetchedAt: tickEpoch})

	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-1" {
			return snapshotWith(97), nil
		}
		return snapshotWith(20), nil
	})
	tick(t, e)

	sib, _ := cacheEntry(t, "u-2")
	if sib.StandDownUntil.IsZero() {
		t.Fatal("nothing stood down, so this test is not about anything")
	}
	got, _ := cacheEntry(t, "u-1")
	if !got.StandDownUntil.IsZero() {
		t.Fatalf("the live account stood itself down until %s — the moment it is switched "+
			"away from, that holds it for half an hour on the one reading worth watching", got.StandDownUntil)
	}
}

// The published deadline is the one the DISPATCHER will honour, which means the
// stand-down applies to an alternate and never to the live account. Both halves
// of the answer are checked: the cache, which is what a daemon that has just
// restarted has, and the engine's own record of what this process scheduled.
func TestThePublishedScheduleIsTheOneTheDispatcherWillHonour(t *testing.T) {
	isolateEngine(t)
	accounts := []store.Account{{UUID: "u-2"}}
	cfg := config.Defaults()
	earned := tickEpoch.Add(pollpolicy.CandidateMaxInterval)
	stood := tickEpoch.Add(pollpolicy.Post429MaxInterval)
	seedEntry(t, "u-2", usage.Entry{
		Snapshot: snapshotWith(20), FetchedAt: tickEpoch,
		NextPollAt: earned, StandDownUntil: stood,
	})
	cache, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	live := func(uuid string) switcher.Evaluation {
		return switcher.Evaluation{Live: store.Account{UUID: uuid}, LiveKnown: true}
	}

	thresholds := configuredThresholds(cfg)

	// A restart: the cache is all there is, and the account is an alternate.
	restarted := NewEngine()
	restarted.publish(accounts, cache, live("u-1"), thresholds, nil)
	if got := nextPollOf(t, restarted.Snapshot(), "u-2"); !got.Equal(stood) {
		t.Errorf("a restarted daemon published %s for a stood-down alternate, want %s — "+
			"the dispatcher will not touch it until then", got, stood)
	}

	// This process polled it while it was an alternate, and it is an alternate
	// still: the stand-down is the answer either way.
	e := NewEngine()
	e.Now = func() time.Time { return tickEpoch }
	e.Rand = func() float64 { return 0.5 }
	e.commit(accounts[0], snapshotWith(20), tickEpoch, []string{"u-2"}, thresholds, false, nil)
	e.publish(accounts, cache, live("u-1"), thresholds, nil)
	if got := nextPollOf(t, e.Snapshot(), "u-2"); !got.Equal(stood) {
		t.Errorf("published %s for a stood-down alternate this process polled, want %s", got, stood)
	}

	// And now it is live. The dispatcher stops honouring the stand-down here, so
	// the record this process kept must stop reporting it.
	reloaded, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := reloaded.Get("u-2")
	e.publish(accounts, reloaded, live("u-2"), thresholds, nil)
	if got := nextPollOf(t, e.Snapshot(), "u-2"); !got.Equal(entry.NextPollAt) {
		t.Errorf("published %s for the account that is now live, want the %s its own poll "+
			"earned — a stand-down written for its predecessor does not hold it", got, entry.NextPollAt)
	}
}

// The poll policy takes its randomness as an argument so that the whole policy
// stays a pure function -- Next's own comment says "the caller passes
// rand.Float64()" -- and NewEngine is the only caller a shipped binary ever
// constructs. With Rand left nil the accessor answers with the midpoint 0.5,
// and jitter(d, 0.5) is d exactly: every guard in pollpolicy is then
// unreachable code, and a fleet that paused together comes back together.
func TestNewEngineSuppliesAJitterSource(t *testing.T) {
	e := NewEngine()
	if e.Rand == nil {
		t.Fatal("NewEngine leaves Rand nil, so every jittered interval is the identity and the spread exists only in the comments")
	}
	seen := map[float64]struct{}{}
	for i := 0; i < 64; i++ {
		v := e.rand()
		if v < 0 || v >= 1 {
			t.Fatalf("rand() = %v, want a sample in [0,1) -- jitter clamps outside it, so an out-of-range source silently pins the spread to one end", v)
		}
		seen[v] = struct{}{}
	}
	if len(seen) < 2 {
		t.Errorf("64 samples produced %d distinct value, which is the constant source this replaces", len(seen))
	}
}

// The property the jitter exists for, stated as what an operator would see. Two
// daemons on two machines, restarted together and handed identical inputs, must
// not choose the same deadline -- they share one per-identity budget of roughly
// 28-30 requests an hour, and coming back in lockstep empties it in one burst.
func TestTwoDaemonsHandedTheSameInputsDoNotPickTheSameDeadline(t *testing.T) {
	a, b := NewEngine(), NewEngine()
	in := pollpolicy.Input{
		Now:       time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC),
		Active:    true,
		Reading:   pollpolicy.Reading{BindingPct: 40, Known: true},
		Threshold: 80,
	}
	const rounds = 32
	agreed := 0
	for i := 0; i < rounds; i++ {
		atA, _ := pollpolicy.Next(pollpolicy.State{}, in, a.rand())
		atB, _ := pollpolicy.Next(pollpolicy.State{}, in, b.rand())
		if atA.Equal(atB) {
			agreed++
		}
	}
	if agreed == rounds {
		t.Errorf("two daemons agreed on the deadline all %d times; a fleet that paused together comes back in one burst", rounds)
	}
}

// midJitter is the sample a test asserting an EXACT deadline has to fix, for the
// same reason it fixes the clock. NewEngine supplies real randomness now, so
// pollpolicy spreads every interval by up to a tenth and no exact time survives
// a run. 0.5 is the midpoint, where jitter is the identity -- which is what
// every deadline in this file was computed against back when the source was nil,
// so pinning it here changes what the tests assert not at all and only says out
// loud which sample they were always relying on.
func midJitter() float64 { return 0.5 }
