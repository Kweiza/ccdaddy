package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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

// codexDispatch must not trust its caller to have already filtered by
// provider. It is only ever handed s.CodexAccounts() today, but a credential
// SHAPE check alone (creds[codexauth.Key]) is not a provider check -- this
// project has already shipped a gate that held on shape rather than on
// provider, and caught it late. The mirror of
// TestACodexAccountWithAClaudeBlobIsNotPollable, which proves the same thing
// about the Claude lane's pollable(): a Claude-provider account carrying a
// codexOAuth-keyed blob must never be polled here.
func TestACodexDispatchNeverPollsAClaudeProviderAccount(t *testing.T) {
	isolateEngine(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{
		Provider: provider.Claude,
		UUID:     "u-claude", Email: "u-claude@example.com",
		AddedAt: tickEpoch.Add(-24 * time.Hour),
	}
	blob := codexauth.Credential{
		AccessToken: "AT-u-claude", RefreshToken: "RT-u-claude",
		AccountID: "acct-u-claude", UserID: "u-claude",
	}.ToBlob()
	if err := s.Add(a, blob); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("u-claude")
	if !ok {
		t.Fatal("seeded account is not in the store")
	}

	polled := false
	e := codexEngine(t,
		func(context.Context, string) (string, string, error) {
			polled = true
			return "AT-u-claude", "acct-u-claude", nil
		},
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			polled = true
			return codexSnapshot(42), codexusage.Identity{}, nil
		})

	e.codexDispatch(context.Background(), s, []store.Account{got}, config.Defaults(), tickEpoch, "")
	e.Wait()

	if polled {
		t.Fatal("a Claude-provider account carrying a codexOAuth blob was polled by the codex lane")
	}
}

// The commit path writes the reading and the series and NOTHING else: no
// ApplyUsage, so a codex snapshot cannot re-file the account's kind, and no
// profile re-read, because there is no Anthropic profile behind it.
func TestTheCodexCommitDoesNotReclassifyTheAccount(t *testing.T) {
	isolateEngine(t)
	before := seedCodexAccount(t, "cx-1")

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
	// The assertion that actually distinguishes this path from the Claude
	// commit, which DOES call store.ApplyUsage: the two checks above hold even
	// if codexCommit secretly called it too, because a present CodexPrimary
	// window already makes identity.ReclassifyOnUsage return KindSubscription
	// -- exactly what seedCodexAccount seeded regardless -- and ApplyUsage
	// never writes Provider at all. Credit is the field ApplyUsage actually
	// writes: a Codex reading carries no ExtraUsage evidence, so a call that
	// reached it would silently stamp Credit.ObservedAt to the poll's time
	// while leaving every other Credit field at its zero value -- exactly the
	// kind of overwrite a future merge of the two lanes could do to a Claude
	// account's credit balance without either lane's own tests noticing.
	if a.Credit != before.Credit {
		t.Fatalf("credit = %+v after a codex poll, want it unchanged from %+v", a.Credit, before.Credit)
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
// `ccdad add codex`.
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

// codexWindows is codexSnapshot with the weekly window filled in too. A poll of
// /wham/usage answers with both, and that is the reading a harvest off one
// forwarded turn has to be merged into rather than written over.
func codexWindows(primary, secondary float64, secondaryReset time.Time) *usage.Snapshot {
	snap := codexSnapshot(primary)
	snap.CodexSecondary = usage.NewWindowWithLength(&secondary, &secondaryReset, 7*24*time.Hour)
	return snap
}

// publishedRow is one account's published engine state, by uuid rather than by
// position: the document lists Claude accounts too, and a test that read
// Accounts[0] would be asserting about whichever row the store happened to
// return first.
func publishedRow(t *testing.T, e *Engine, uuid string) AccountStatus {
	t.Helper()
	for _, row := range e.Snapshot().Accounts {
		if row.UUID == uuid {
			return row
		}
	}
	t.Fatalf("%s has no published row", uuid)
	return AccountStatus{}
}

// The whole point of the harvest: a reading the proxy took off a turn the user
// was making anyway is committed by the lane, and the poll that would have
// bought the same figure is not made.
//
// It is asserted through a tick rather than through the two accessors it is
// built from, because the accessors were the only thing tested: deleting the
// branch in codexDispatch that consumes a harvested reading left this package
// green, and a lane that never consumed one would put Codex quota back to being
// up to fifteen minutes stale with nothing to show for it.
func TestAHarvestedReadingIsCommittedWithoutAPoll(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	polls := 0
	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			polls++
			return codexSnapshot(1), codexusage.Identity{}, nil
		})
	e.harvestCodexSample("cx-1", codexSnapshot(90))
	tick(t, e)

	got, ok := cacheEntry(t, "cx-1")
	if !ok {
		t.Fatal("the tick cached nothing, though a harvested reading was waiting")
	}
	if pct, known := got.Snapshot.CodexPrimary.Percent(); !known || pct != 90 {
		t.Fatalf("cached utilization = %v (%v), want the harvested 90", pct, known)
	}
	if polls != 0 {
		t.Fatalf("the lane made %d poll(s) for an account whose reading it already had", polls)
	}
	if _, ok := e.CodexSample("cx-1"); ok {
		t.Fatal("the harvested reading is still held after the tick that committed it; it would be re-committed on every tick until another replaced it")
	}
}

// A poll that was already in flight when the proxy harvested a fresher reading
// must not land on top of it.
//
// Measured before the claim moved above the harvest branch: a poll parked in
// CodexFetchUsage held no lock over the cache row it was going to write, so the
// next tick committed the harvested 95% under it and the poll then wrote its own
// 10% over that. The lane went on ranking a nearly spent account first, and the
// 95% reading was gone rather than deferred -- CodexSample deletes as it hands
// out. FetchedAt cannot untangle the two afterwards either: it is stamped when
// the commit runs, so the late poll's is the larger of the two.
func TestAPollAlreadyInFlightCannotOverwriteAHarvestedReading(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	release := make(chan struct{})
	polls := 0
	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			polls++
			<-release
			return codexSnapshot(10), codexusage.Identity{}, nil
		})
	// Not tick(): that waits for the fleet, and this poll is parked on purpose.
	// The claim is taken on the tick's own goroutine, so it is already held by
	// the time Tick returns.
	if err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	e.harvestCodexSample("cx-1", codexSnapshot(95))
	if err := e.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	close(release)
	e.Wait()

	tick(t, e)
	got, ok := cacheEntry(t, "cx-1")
	if !ok {
		t.Fatal("the lane cached nothing at all")
	}
	if pct, known := got.Snapshot.CodexPrimary.Percent(); !known || pct != 95 {
		t.Fatalf("cached utilization = %v (%v), want the harvested 95: the poll that answered 10 read the account BEFORE the turn the proxy took 95 off, and the lane now believes a spent account is roomy for a whole codex floor", pct, known)
	}
	if polls != 1 {
		t.Fatalf("the lane made %d polls, want the one this test parked", polls)
	}
}

// A harvested reading carries whatever the answer to one turn happened to
// carry, and a response can name the primary window without the secondary --
// codexproxy publishes a sample when EITHER family is present, because the
// family on a 429 is the most informative reading there is and it is often
// primary-only. Written over the cached reading wholesale, such a sample ERASED
// the weekly: a 96%-spent account with 4% of room re-read as 90% of room on the
// five-hour window, kept its place at the top of the lane's ranking, and went on
// being served into a wall of 429s.
func TestAPartialHarvestKeepsTheWindowItDidNotCarry(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	now := tickEpoch
	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return codexWindows(10, 96, tickEpoch.Add(72*time.Hour)), codexusage.Identity{}, nil
		})
	e.Now = func() time.Time { return now }
	tick(t, e)

	now = tickEpoch.Add(time.Minute)
	e.harvestCodexSample("cx-1", codexSnapshot(90))
	tick(t, e)

	got, _ := cacheEntry(t, "cx-1")
	if pct, ok := got.Snapshot.CodexPrimary.Percent(); !ok || pct != 90 {
		t.Fatalf("the five-hour window reads %v (%v) after the harvest, want the harvested 90", pct, ok)
	}
	pct, ok := got.Snapshot.CodexSecondary.Percent()
	if !ok || pct != 96 {
		t.Fatalf("the weekly window reads %v (%v) after a primary-only harvest, want the cached 96 carried forward: erasing it turns 4%% of room into 90%% and leaves the lane ranking a spent account first", pct, ok)
	}
	// The carried window is a fact about the cache row, not about this turn. The
	// usage series is a record of what was OBSERVED, and a point claiming the
	// weekly was read at this instant would flatten the burn rate across a gap
	// nothing measured.
	series := seriesOf(t, "cx-1")
	if len(series) == 0 {
		t.Fatal("the harvest appended no sample to the usage series")
	}
	last := series[len(series)-1]
	if !last.At.Equal(now) {
		t.Fatalf("the last sample is stamped %s, want the harvest's %s", last.At, now)
	}
	if _, ok := last.Windows[usage.WindowCodexSecondary]; ok {
		t.Fatal("the weekly window carried forward from the cache was appended to the usage series as though this turn had read it")
	}
}

// The carry is bounded by the window's own reset. A weekly that has rolled over
// since it was read is not evidence about the window running now, and carrying
// its 96% forward would hold an account out of rotation through quota it has
// already got back.
func TestAWindowThatHasRolledOverIsNotCarriedIntoAHarvest(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	now := tickEpoch
	rollover := tickEpoch.Add(2 * time.Hour)
	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return codexWindows(10, 96, rollover), codexusage.Identity{}, nil
		})
	e.Now = func() time.Time { return now }
	tick(t, e)

	now = rollover.Add(time.Minute)
	e.harvestCodexSample("cx-1", codexSnapshot(90))
	tick(t, e)

	got, _ := cacheEntry(t, "cx-1")
	if pct, ok := got.Snapshot.CodexSecondary.Percent(); ok {
		t.Fatalf("the weekly reads %v after its own reset at %s passed, want it absent rather than carried: the window it described has ended", pct, rollover)
	}
	if got.Snapshot.CodexSecondary.Present {
		t.Fatal("a window whose reset has passed was carried forward as present")
	}
}

// A harvested reading is evidence the account was REACHED, and the record of
// the last poll is what the status document and the TUI print when they say
// ccdad cannot reach it.
//
// Measured before this: one poll that failed on a flaky network, then a busy
// session in which every reading arrived through the proxy before the next
// NextPollAt came round, and `ccdad status --json` went on reporting "network is
// unreachable" for an account it was reading successfully every few minutes.
//
// LastPollAt is deliberately NOT moved. It says when the daemon last polled, and
// a harvest is not a poll: stamping it would report a request to /wham/usage
// that was never made.
func TestAHarvestedReadingClearsTheErrorOfAPollThatFailed(t *testing.T) {
	isolateEngine(t)
	seedCodexAccount(t, "cx-1")

	now := tickEpoch
	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return nil, codexusage.Identity{}, errors.New("dial tcp 172.64.155.209:443: connect: network is unreachable")
		})
	e.Now = func() time.Time { return now }
	tick(t, e)

	before := publishedRow(t, e, "cx-1")
	if before.LastPollError == "" {
		t.Fatal("the failed poll recorded no error, so this test would pass on a daemon that never clears one")
	}

	// The clock MOVES between the failed poll and the harvest, or the second
	// assertion below is blind: a commit that stamped LastPollAt from a frozen
	// clock would write back the instant the poll already recorded, and moving
	// the field would be indistinguishable from leaving it alone.
	now = tickEpoch.Add(30 * time.Minute)
	e.harvestCodexSample("cx-1", codexSnapshot(34))
	tick(t, e)

	got := publishedRow(t, e, "cx-1")
	if got.LastPollError != "" {
		t.Fatalf("the status document still says %q for an account the proxy read a moment ago", got.LastPollError)
	}
	if !got.LastPollAt.Equal(before.LastPollAt) {
		t.Fatalf("LastPollAt moved from %s to %s on a harvest; no request to /wham/usage was made, and the field says when the daemon last polled", before.LastPollAt, got.LastPollAt)
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

// The default arm of codexTick's three-way switch: a PLAIN Execute failure,
// where the pointer never moves at all. codex/ ITSELF is chmod'd unwritable
// here, rather than the store root -- mirroring TestAFailedPointerWriteStampsNothing
// in internal/codexswitch -- so the pointer write fails before the stamp step
// is ever reached, and this cannot land on the ErrPointerMovedUnstamped arm the
// way TestARepointWithAFailedStampStillPublishesTheNewAccount above does. ev
// must be left exactly as EvaluateCodex returned it: Live still names the
// account that was serving before this tick ran, because what is actually
// being served has not changed.
func TestAPlainRepointFailureLeavesTheEvaluationAtThePreSwitchAccount(t *testing.T) {
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

	// codex/ already exists from the Execute call above, and store.Open never
	// touches it -- only the store's own directories -- so chmod'ing it here
	// survives past the store.Open call above, unlike chmod'ing root does.
	codexDir := filepath.Dir(codexswitch.ServingPath(root))
	if err := os.Chmod(codexDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(codexDir, 0o700) })

	ev, tickErr := e.codexTick(context.Background(), s, config.Defaults(), tickEpoch)
	e.Wait()
	if tickErr != nil {
		t.Fatalf("codexTick: %v, want nil (a plain Execute failure is logged, not returned)", tickErr)
	}

	if got := servingUUID(t); got != "cx-1" {
		t.Fatalf("serving = %q, want cx-1 (the pointer write itself failed, so it must not have moved)", got)
	}
	if !ev.LiveKnown || ev.Live.UUID != "cx-1" {
		t.Fatalf("codexTick's own evaluation: Live = %+v, LiveKnown = %v, want cx-1 (the pre-switch account) known",
			ev.Live, ev.LiveKnown)
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

	// Mirrors TestATokenInsideAnHourOfExpiryIsRefreshedBeforeThePoll's own
	// seeding: the store's stored credential has to be the SAME token the mock
	// hands back, or codexauth.Refresher.Refresh's own re-read never agrees
	// with what it was told to refresh, short-circuits to Adopted before
	// reaching the transport, and refusingTokenEndpoint never has a chance to
	// fire either way -- which would make it decoration rather than a live
	// assertion.
	fresh := codexJWT(tickEpoch.Add(6 * time.Hour))
	if err := store.WithStore(func(s *store.Store) error {
		return s.SetCredentials("cx-1", codexauth.Credential{
			AccessToken: fresh, RefreshToken: "RT-cx-1", AccountID: "acct-cx-1", UserID: "cx-1",
		}.ToBlob())
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var sawToken string
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

// `ccdad status --refresh` is a CLI process, and the whole of what it may do to a
// Codex account is read it with the token already stored. It is the SAME poller
// the tick dispatches, through the same commit into the same cache, which is
// what keeps `list` and `status --json` from computing two schedules.
func TestRefreshPollsACodexAccountWithTheStoredToken(t *testing.T) {
	isolateEngine(t)
	a := seedCodexAccount(t, "cx-1")

	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return codexSnapshot(42), codexusage.Identity{}, nil
		})
	if e.CodexRefresher != nil {
		t.Fatal("the fixture gave a CLI-shaped engine a refresher; only the daemon may hold one")
	}

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	res := e.Refresh(context.Background(), s, []store.Account{a}, config.Defaults(), "")
	if len(res) != 1 || res[0].State != RefreshFetched {
		t.Fatalf("Refresh = %v, want one fetched row", res)
	}
	got, ok := cacheEntry(t, "cx-1")
	if !ok {
		t.Fatal("the refresh cached no codex reading")
	}
	if pct, known := got.Snapshot.CodexPrimary.Percent(); !known || pct != 42 {
		t.Fatalf("cached utilization = %v (%v), want 42", pct, known)
	}
}

// A 401 leaves the row stale and marks NOTHING.
//
// The daemon's lane answers a 401 by asking the refresher, and a Terminal
// outcome there is what sets CodexReloginFor. A CLI process has no refresher,
// so it has not tested the grant at all -- it has only been told that one
// access token is stale. Marking on that evidence would put an account out of
// rotation on a verdict nobody reached.
func TestARefreshRejectedWithA401MarksNothing(t *testing.T) {
	isolateEngine(t)
	a := seedCodexAccount(t, "cx-1")

	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return nil, codexusage.Identity{}, &usage.StatusError{Status: 401}
		})

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	res := e.Refresh(context.Background(), s, []store.Account{a}, config.Defaults(), "")
	if len(res) != 1 || res[0].State != RefreshFailed {
		t.Fatalf("Refresh = %v, want one failed row", res)
	}
	after, ok := s.Get("cx-1")
	if !ok {
		t.Fatal("cx-1 left the store")
	}
	if after.CodexReloginFor != "" {
		t.Fatalf("a CLI refresh marked %s as needing a login on the strength of one 401", after.UUID)
	}
}

// The rule this whole arm exists to keep: a hand-triggered refresh reads with
// the token it already holds and NEVER rotates. A grant is single-use, the
// daemon is already the one spender, and a second process rotating alongside it
// invalidates the refresh token server-side and logs the user out with no undo.
//
// Wiring discipline is not the guard. Every symbol needed to set a refresher on
// a CLI-built engine is exported and already imported over there, so this test
// puts a WORKING one on the engine -- backed by the same rotating endpoint the
// lane's own tests use -- and asserts the stored credential is untouched
// afterwards. Without the refusal at the top of refreshCodex the poll rotates,
// the store comes back holding AT-rotated, and both of the assertions in the
// test above stay green while it happens.
func TestAHandTriggeredRefreshWithARefresherRefusesInsteadOfRotating(t *testing.T) {
	isolateEngine(t)
	a := seedCodexAccount(t, "cx-1")

	before, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	was, err := before.Credentials("cx-1")
	if err != nil {
		t.Fatalf("cx-1 has no credential to protect: %v", err)
	}

	e := codexEngine(t, codexTokensAreFine,
		func(context.Context, string, string) (*usage.Snapshot, codexusage.Identity, error) {
			return nil, codexusage.Identity{}, &usage.StatusError{Status: 401}
		})
	e.CodexRefresher = codexauth.NewRefresher(codexauth.RefresherConfig{
		Client: &http.Client{Transport: rotatingTokenEndpoint{}},
	})

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	res := s2res(e.Refresh(context.Background(), s, []store.Account{a}, config.Defaults(), ""))
	if res.State != RefreshFailed {
		t.Fatalf("State = %v, want the refusal to report a failed row", res.State)
	}
	if res.Err == nil {
		t.Fatal("the refusal reported no error; a caller that wired a refresher must be told")
	}

	now, err := s.Credentials("cx-1")
	if err != nil {
		t.Fatalf("cx-1 lost its credential: %v", err)
	}
	if !reflect.DeepEqual(was, now) {
		t.Fatal("the hand-triggered refresh rotated the grant; the daemon is the only spender")
	}
}

func s2res(rs []RefreshResult) RefreshResult {
	if len(rs) != 1 {
		panic("want exactly one row")
	}
	return rs[0]
}

// An engine built without the read seams answers unpollable rather than
// failing. That is what every process that is not the daemon and did not ask
// for them looks like, and `list` prints nothing about an unpollable row.
func TestARefreshWithNoCodexSeamsIsUnpollable(t *testing.T) {
	isolateEngine(t)
	a := seedCodexAccount(t, "cx-1")

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return nil, errors.New("no Claude account in this test should be polled")
	})

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	res := e.Refresh(context.Background(), s, []store.Account{a}, config.Defaults(), "")
	if len(res) != 1 || res[0].State != RefreshUnpollable {
		t.Fatalf("Refresh = %v, want one unpollable row", res)
	}
}
