package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexproxy"
	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/codexusage"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// seedCodexAccount stores a Codex account with the credential blob the lane's
// eligibility rule looks for. AddedAt is placed before the frozen clock for the
// reason seedAccount does it: Cache.Prune drops a reading dated before its
// account was added.
func seedCodexAccount(t *testing.T, uuid string) store.Account {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{
		UUID: uuid, Email: uuid + "@example.com",
		Provider: provider.Codex, Kind: identity.KindSubscription,
		AddedAt: tickEpoch.Add(-24 * time.Hour),
	}
	blob := codexauth.Credential{
		AccessToken: "AT-" + uuid, RefreshToken: "RT-" + uuid,
		AccountID: "acct-" + uuid, UserID: uuid,
	}.ToBlob()
	if err := s.Add(a, blob); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(uuid)
	return got
}

// codexSnapshot is a reading whose codex five-hour window is `used` percent
// spent. It is the Codex twin of snapshotWith.
func codexSnapshot(used float64) *usage.Snapshot {
	resets := tickEpoch.Add(time.Hour)
	return &usage.Snapshot{
		CodexPrimary: usage.NewWindowWithLength(&used, &resets, 5*time.Hour),
	}
}

// codexEngine is engineFor's twin: a frozen clock, pinned jitter, and the two
// Codex seams faked. Every Claude seam is left as engineFor leaves it, so a
// codex test cannot reach Anthropic's endpoints by forgetting a stub.
func codexEngine(t *testing.T,
	token func(context.Context, string) (string, string, error),
	fetch func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error)) *Engine {

	t.Helper()
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return nil, errors.New("no Claude account in this test should be polled")
	})
	e.CodexAccessToken = token
	e.CodexFetchUsage = fetch
	e.CodexBook = &codexproxy.LimitBook{}
	return e
}

func codexTokensAreFine(_ context.Context, uuid string) (string, string, error) {
	return "AT-" + uuid, "acct-" + uuid, nil
}

func servingUUID(t *testing.T) string {
	t.Helper()
	uuid, _ := codexswitch.ReadServing(mustPath(ccpath.StoreHome()))
	return uuid
}

// A tick reaches the Codex table, caches the reading, and does it without
// touching Claude Code's credentials file.
func TestATickPollsACodexAccountAndCachesTheReading(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return codexSnapshot(42), codexusage.Identity{}, nil
		})
	tick(t, e)

	got, ok := cacheEntry(t, "cx-1")
	if !ok {
		t.Fatal("the tick cached no codex reading")
	}
	if pct, ok := got.Snapshot.CodexPrimary.Percent(); !ok || pct != 42 {
		t.Fatalf("cached utilization = %v (%v), want 42", pct, ok)
	}
	if !got.FetchedAt.Equal(tickEpoch) {
		t.Fatalf("FetchedAt = %s, want %s", got.FetchedAt, tickEpoch)
	}
	assertNoLiveCredentialsWritten(t)
}

// The floor is fifteen minutes, not the Claude table's three. A Codex reading
// moves slowly and the endpoint is not one ccdad has measured an allowance for.
func TestACodexPollIsScheduledOnTheCodexTable(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return codexSnapshot(42), codexusage.Identity{}, nil
		})
	tick(t, e)

	got, _ := cacheEntry(t, "cx-1")
	if gap := got.NextPollAt.Sub(tickEpoch); gap < 15*time.Minute {
		t.Fatalf("the next codex poll is %v out, want at least the codex floor of 15m", gap)
	}
}

// The whole point of the lane: a reading arrives, the lane ranks on it, and the
// pointer moves with nobody watching.
func TestATickGoesFromACodexReadingToARepoint(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")
	seedCodexAccount(t, "cx-2")
	if err := codexswitch.Execute(mustPath(ccpath.StoreHome()), "cx-1"); err != nil {
		t.Fatal(err)
	}
	clearCodexStamp(t)

	e := codexEngine(t, codexTokensAreFine,
		func(_ context.Context, token, _ string) (*usage.Snapshot, codexusage.Identity, error) {
			if token == "AT-cx-1" {
				return codexSnapshot(90), codexusage.Identity{}, nil
			}
			return codexSnapshot(10), codexusage.Identity{}, nil
		})
	tick(t, e) // fills the cache
	tick(t, e) // ranks on it

	if got := servingUUID(t); got != "cx-2" {
		t.Fatalf("serving = %q, want cx-2", got)
	}
	if got := e.Snapshot().CodexServingUUID; got != "cx-2" {
		t.Fatalf("published CodexServingUUID = %q, want cx-2", got)
	}
}

// An account another machine drives is not polled here unless it is the one
// serving: the reading spends a budget somebody else is driving, and this
// machine can never rank it.
func TestAnElsewhereCodexAccountIsNotPolled(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")
	seedCodexAccount(t, "cx-2")
	if err := store.WithStore(func(s *store.Store) error {
		_, err := s.SetOwned([]string{"cx-1"})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	polled := map[string]bool{}
	e := codexEngine(t, codexTokensAreFine,
		func(_ context.Context, token, _ string) (*usage.Snapshot, codexusage.Identity, error) {
			mu.Lock()
			polled[token] = true
			mu.Unlock()
			return codexSnapshot(42), codexusage.Identity{}, nil
		})
	tick(t, e)

	mu.Lock()
	defer mu.Unlock()
	if polled["AT-cx-2"] {
		t.Fatal("an account another machine drives was polled")
	}
	if !polled["AT-cx-1"] {
		t.Fatal("the account this machine drives was not polled")
	}
}

// The commit path writes the reading and the series and NOTHING else: no
// ApplyUsage, so a codex snapshot cannot re-file the account's kind, and no
// profile re-read, because there is no Anthropic profile behind it.
func TestTheCodexCommitDoesNotReclassifyTheAccount(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return codexSnapshot(42), codexusage.Identity{}, nil
		})
	// A profile seam that must never be reached from this lane.
	e.FetchProfile = func(context.Context, string) (*identity.Profile, error) {
		t.Fatal("the codex lane asked for an Anthropic profile")
		return nil, nil
	}
	tick(t, e)

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get("cx-1")
	if a.Kind != identity.KindSubscription {
		t.Fatalf("kind = %v after a codex poll, want it unchanged", a.Kind)
	}
	if a.Provider != provider.Codex {
		t.Fatalf("provider = %q after a codex poll, want codex", a.Provider)
	}
}

// A 429 marks the account in the book the proxy shares, so the two halves agree
// about which account is throttled without either polling the other.
func TestACodexRateLimitReachesTheSharedBook(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return nil, codexusage.Identity{}, &usage.StatusError{Status: 429}
		})
	tick(t, e)

	if _, limited := e.CodexBook.LimitedUntil("cx-1", tickEpoch); !limited {
		t.Fatal("a 429 from the codex usage endpoint did not reach the shared book")
	}
}

// An account whose grant is dead is not polled: every request would spend a
// round trip to be told the same thing, and the remedy is a person running
// `ccdad codex add`.
func TestAnAccountNeedingALoginIsNotPolled(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")
	if err := store.WithStore(func(s *store.Store) error {
		h := codexauth.RefreshTokenHash("RT-cx-1")
		return s.SetCodexReloginFor("cx-1", h, h)
	}); err != nil {
		t.Fatal(err)
	}

	polled := false
	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			polled = true
			return codexSnapshot(42), codexusage.Identity{}, nil
		})
	tick(t, e)

	if polled {
		t.Fatal("an account whose grant is dead was polled")
	}
	if got := e.Snapshot().Accounts[0].State; got != StateNeedsRelogin {
		t.Fatalf("state = %v, want %v", got, StateNeedsRelogin)
	}
}

// A machine with no Codex accounts must reach none of this, and in particular
// must not read a pointer file, take the state lock, or publish a serving uuid.
func TestAMachineWithNoCodexAccountsPublishesNoServingAccount(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAs(t, "u-1")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(42), nil
	})
	tick(t, e)

	if got := e.Snapshot().CodexServingUUID; got != "" {
		t.Fatalf("CodexServingUUID = %q on a machine with no codex accounts", got)
	}
}

// The three-way outcome of codexswitch.Execute: when the pointer moves but the
// cooldown stamp fails, Execute returns an error wrapping
// ErrPointerMovedUnstamped -- but the pointer HAS moved. The lane must fold
// that move into the evaluation it hands to publish, or the status document
// names the account that WAS serving while the machine actually serves the
// new one.
//
// The store root is chmod'd read-only AFTER the store is opened and the cache
// is seeded, and BEFORE codexTick runs. That reproduces the split Execute's
// own package proves is possible (TestAFailedStampLeavesThePointerMovedAndSaysSo
// in internal/codexswitch): codex/ was created earlier and stays writable, so
// the pointer write succeeds, but strategy.json.lock is a fresh directory that
// has to be created directly under root, so the cooldown stamp fails. A store
// re-open would silently chmod root back to 0700 -- store.Open tightens every
// store directory's mode on every call -- which is why this test keeps the one
// *store.Store it opened before the chmod rather than reopening one.
func TestARepointWithAFailedStampStillPublishesTheNewAccount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows beyond the read-only bit")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")
	seedCodexAccount(t, "cx-2")
	root := mustPath(ccpath.StoreHome())
	if err := codexswitch.Execute(root, "cx-1"); err != nil {
		t.Fatal(err)
	}
	clearCodexStamp(t)

	// Seed the cache directly rather than through a poll: codexDue reads these
	// as fresh at tickEpoch, so codexTick's own dispatch has nothing to do
	// after it ranks -- and nothing here needs to touch the token or fetch
	// seams, which are stubbed to fail the test if either is called.
	if err := usage.WithCache(cacheTimeout, func(c *usage.Cache) error {
		c.Put("cx-1", usage.Entry{Snapshot: codexSnapshot(90), FetchedAt: tickEpoch})
		c.Put("cx-2", usage.Entry{Snapshot: codexSnapshot(10), FetchedAt: tickEpoch})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	e := codexEngine(t,
		func(context.Context, string) (string, string, error) {
			t.Fatal("nothing should be polled: both cache entries are fresh at tickEpoch")
			return "", "", nil
		},
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			t.Fatal("nothing should be polled: both cache entries are fresh at tickEpoch")
			return nil, codexusage.Identity{}, nil
		})

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	ev, tickErr := e.codexTick(context.Background(), s, config.Defaults(), tickEpoch)
	e.Wait()
	if tickErr != nil {
		t.Fatalf("codexTick: %v (its own error return is for evaluation failures, not a logged Execute failure)", tickErr)
	}

	if got := servingUUID(t); got != "cx-2" {
		t.Fatalf("serving = %q, want cx-2 (the pointer must have moved despite the stamp failing)", got)
	}
	if !ev.LiveKnown || ev.Live.UUID != "cx-2" {
		t.Fatalf("codexTick's own evaluation: Live = %+v, LiveKnown = %v, want cx-2 known", ev.Live, ev.LiveKnown)
	}
	if st, lerr := strategy.LoadState(); lerr == nil {
		if _, to := st.CodexLastSwitch(); to == "cx-2" {
			t.Fatal("the cooldown was stamped despite the unwritable root; this test proves nothing about outcome 2")
		}
	}

	e.publish(s.Accounts(), &usage.Cache{}, switcher.Evaluation{}, ev, configuredThresholds(config.Defaults()), map[string]bool{})
	if got := e.Snapshot().CodexServingUUID; got != "cx-2" {
		t.Fatalf("published CodexServingUUID = %q after a stamp-only failure, want cx-2", got)
	}
}

// clearCodexStamp removes the cooldown codexswitch.Execute leaves behind, for
// the tests that are about the lane's decision rather than about the hold.
func clearCodexStamp(t *testing.T) {
	t.Helper()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.RecordCodexSwitch("", time.Time{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// assertNoLiveCredentialsWritten is the never-cross assertion in this package:
// a codex tick must leave Claude Code's credentials file exactly as it found it.
func assertNoLiveCredentialsWritten(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(mustPath(ccpath.CredentialsPath())); !os.IsNotExist(err) {
		t.Fatalf("the codex lane wrote Claude Code's credentials file at %s",
			mustPath(ccpath.CredentialsPath()))
	}
}

// codexJWT is an access token whose only claim is an expiry. The proactive
// refresh reads exactly that claim and nothing else, so a three-part token with
// a one-field payload is the whole fixture -- and it is built here rather than
// pasted so the instant is relative to the frozen clock.
func codexJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." +
		enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".c2ln"
}

// A token an hour from its expiry is rotated by the daemon lane BEFORE the poll
// spends it, so a codex turn never has to eat the 401 that would otherwise be
// what triggers a rotation. No other process may do this: the endpoint kills a
// refresh token that is used twice, so a CLI that rotated would be a second
// spender of one grant.
func TestATokenInsideAnHourOfExpiryIsRefreshedBeforeThePoll(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	// The near-expiry token has to be what is actually stored, exactly as it
	// would be in production: e.CodexAccessToken always hands back what is on
	// disk, and the refresher's own re-read (codexauth.Refresher.Refresh,
	// through readCodexCredential) compares its triggeredBy argument against
	// THAT value to tell "I am the one refreshing this" from "somebody else
	// already rotated it while I was mid-call". A mock that returned a token
	// the store never held would make every refresh here read as the second
	// case (Adopted) rather than the first, and adopt the untouched token
	// right back.
	near := codexJWT(tickEpoch.Add(30 * time.Minute))
	if err := store.WithStore(func(s *store.Store) error {
		return s.SetCredentials("cx-1", codexauth.Credential{
			AccessToken: near, RefreshToken: "RT-cx-1", AccountID: "acct-cx-1", UserID: "cx-1",
		}.ToBlob())
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var sawToken string
	e := codexEngine(t,
		func(context.Context, string) (string, string, error) {
			// Thirty minutes out, which is inside the hour, and the same value
			// just stored.
			return near, "acct-cx-1", nil
		},
		func(_ context.Context, token, _ string) (*usage.Snapshot, codexusage.Identity, error) {
			mu.Lock()
			sawToken = token
			mu.Unlock()
			return codexSnapshot(42), codexusage.Identity{}, nil
		})
	e.CodexRefresher = codexauth.NewRefresher(codexauth.RefresherConfig{
		Client: &http.Client{Transport: rotatingTokenEndpoint{}},
	})
	tick(t, e)

	mu.Lock()
	defer mu.Unlock()
	if sawToken != "AT-rotated" {
		t.Fatalf("the poll spent %q, want the rotated token AT-rotated", sawToken)
	}
}

// A token with hours left is NOT rotated. A grant is single-use, so refreshing
// one that does not need it spends the fleet's grants for nothing.
func TestATokenWithHoursLeftIsNotRefreshed(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	var mu sync.Mutex
	var sawToken string
	fresh := codexJWT(tickEpoch.Add(6 * time.Hour))
	e := codexEngine(t,
		func(context.Context, string) (string, string, error) { return fresh, "acct-cx-1", nil },
		func(_ context.Context, token, _ string) (*usage.Snapshot, codexusage.Identity, error) {
			mu.Lock()
			sawToken = token
			mu.Unlock()
			return codexSnapshot(42), codexusage.Identity{}, nil
		})
	e.CodexRefresher = codexauth.NewRefresher(codexauth.RefresherConfig{
		Client: &http.Client{Transport: refusingTokenEndpoint{t: t}},
	})
	tick(t, e)

	mu.Lock()
	defer mu.Unlock()
	if sawToken != fresh {
		t.Fatalf("the poll spent %q, want the stored token unchanged", sawToken)
	}
}

// rotatingTokenEndpoint answers codexauth.TokenURL with a fresh pair and
// refuses every other host, so a test that reached the real endpoint fails
// rather than succeeding quietly.
type rotatingTokenEndpoint struct{}

func (rotatingTokenEndpoint) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() != codexauth.TokenURL {
		return nil, fmt.Errorf("this test reached %s; only the token endpoint is stubbed", r.URL)
	}
	body := `{"access_token":"AT-rotated","refresh_token":"RT-rotated"}`
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    r,
	}, nil
}

// refusingTokenEndpoint fails the test if anything asks it to rotate. It is the
// assertion in TestATokenWithHoursLeftIsNotRefreshed; the return value only
// keeps the transport contract.
type refusingTokenEndpoint struct{ t *testing.T }

func (rt refusingTokenEndpoint) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.t.Errorf("a token with hours left was sent to the token endpoint (%s)", r.URL)
	return nil, errors.New("no rotation was expected")
}
