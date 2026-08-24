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
	tick(t, e)

	if _, ok := cacheEntry(t, "u-key"); ok {
		t.Fatal("a token account got a cache entry")
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
			state := accountState(store.Account{UUID: "u-1"}, cache, false, "", cfg)

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
		e.commit(a, windowsUsed(10, 50), tickEpoch, []string{a.UUID}, cfg, true, nil)
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

	now := tickEpoch
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		if token == "AT-u-1" {
			return snapshotWith(97), nil
		}
		return snapshotWith(20), nil
	})
	e.Now = func() time.Time { return now }

	// The first tick gives every account an entry. The second is the one that
	// matters: only the live account is due by then, so nothing else is writing.
	tick(t, e)
	second := tickEpoch.Add(pollpolicy.UrgentInterval + time.Second)
	now = second
	tick(t, e)

	sib, ok := cacheEntry(t, "u-2")
	if !ok {
		t.Fatal("the account sharing the identity has no entry")
	}
	if want := second.Add(pollpolicy.Post429MaxInterval); !sib.StandDownUntil.Equal(want) {
		t.Fatalf("StandDownUntil = %s, want %s", sib.StandDownUntil, want)
	}
	// The schedule its own poll earned is untouched. A stand-down that overwrote
	// it would overwrite a 429's floor with something shorter.
	if want := tickEpoch.Add(2 * pollpolicy.CandidateMaxInterval); !sib.NextPollAt.Equal(want) {
		t.Fatalf("NextPollAt = %s, want the %s its own poll set", sib.NextPollAt, want)
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
