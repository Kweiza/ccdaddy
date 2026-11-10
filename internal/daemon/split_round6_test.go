package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Round 6: the hold lifts as soon as the stored credential is no longer the
// SPENT one -- whatever replaced it -- and still holds while it is (see
// TestTheHoldStaysWhileTheSpentLoginIsStillStored for the stores agreeing
// while the spent login is still stored). A probe asks the hold too.

func TestTheHoldLiftsWhenALaterGenerationIsStored(t *testing.T) {
	isolateEngine(t)
	a := seedAccount(t, "u-1", "org-1")
	e := engineFor(t, tokensAreFine, noFetch)
	// B1 was named and missed; B2, a later refresh, is what gets stored.
	e.holdUntilStored("u-1", "refresh:RT-u-1-b1", "refresh:RT-u-1")
	if err := openEngineStore(t).Add(a, oauthBlob("RT-u-1-b2")); err != nil {
		t.Fatal(err)
	}
	e.releaseStored(openEngineStore(t))
	if err := e.holdSpentToken("u-1"); err != nil {
		t.Fatalf("the hold stayed after a later generation replaced the spent login: %v", err)
	}
}

func TestTheHoldLiftsWhenTheAccountIsLoggedInAgain(t *testing.T) {
	isolateEngine(t)
	a := seedAccount(t, "u-1", "org-1")
	e := engineFor(t, tokensAreFine, noFetch)
	e.holdUntilStored("u-1", "refresh:RT-u-1-newer", "refresh:RT-u-1")
	// `ccdad add` stores a brand-new grant.
	if err := openEngineStore(t).Add(a, oauthBlob("RT-u-1-relogin")); err != nil {
		t.Fatal(err)
	}
	e.releaseStored(openEngineStore(t))
	if err := e.holdSpentToken("u-1"); err != nil {
		t.Fatalf("the hold stayed after a re-login replaced the spent login: %v", err)
	}
}

// A switch installs the target's STORED login into Claude Code's stores, and
// Claude Code then refreshes with it: while that token is spent, no swap
// installs it.
func TestNoSwitchInstallsAHeldStoredLogin(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	target := seedAccount(t, "u-2", "org-2")
	liveAs(t, "u-1")
	before := fileNow(t)
	e := engineFor(t, tokensAreFine, noFetch)
	e.holdUntilStored("u-2", "refresh:RT-u-2-newer", "refresh:RT-u-2")

	res, err := e.execute(context.Background(), openEngineStore(t), switcher.Request{Target: target, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome == switcher.Switched || fileNow(t) != before {
		t.Fatalf("outcome = %v: a held stored login was installed", res.Outcome)
	}
}

// A probe seeds a scratch Claude Code home with the account's STORED login,
// which refreshes with the stored refresh token: while that is spent, no probe
// is due.
func TestNoProbeIsDueWhileTheStoredTokenIsHeld(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	e.Now = func() time.Time { return tickEpoch }
	a := store.Account{UUID: "u-1", Provider: provider.Claude, SubscriptionStatus: "active"}
	entry := usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)}
	cfg := config.Defaults()
	thresholds := configuredThresholds(cfg)
	if _, _, want := e.probeDue(a, entry, cfg, thresholds, tickEpoch, "", true, nil); !want {
		t.Fatal("precondition: the probe is not due without a hold")
	}
	e.holdUntilStored("u-1", "refresh:RT-u-1-newer", "refresh:RT-u-1")
	if _, _, want := e.probeDue(a, entry, cfg, thresholds, tickEpoch, "", true, nil); want {
		t.Fatal("a probe is due for an account whose stored refresh token is spent")
	}
}
