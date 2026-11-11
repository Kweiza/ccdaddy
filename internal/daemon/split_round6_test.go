package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Round 6: the hold lifts as soon as something issued no earlier than the
// newer login is stored -- whatever it is -- and still holds while the spent
// login is (see TestTheHoldStaysWhileTheSpentLoginIsStillStored for the stores
// agreeing while the spent login is still stored). A probe asks the hold too.

func TestTheHoldLiftsWhenALaterGenerationIsStored(t *testing.T) {
	isolateEngine(t)
	a := seedAccount(t, "u-1", "org-1")
	e := engineFor(t, tokensAreFine, noFetch)
	// The newer login was named and missed; a later refresh is what gets
	// stored.
	spendStoredLogin(t, e, "u-1")
	storeLogin(t, a, blobExpiring("RT-u-1-later", tickEpoch.Add(10*time.Hour)))
	passSees(t, e)
	if err := e.holdSpentToken("u-1"); err != nil {
		t.Fatalf("the hold stayed after a later generation replaced the spent login: %v", err)
	}
}

func TestTheHoldLiftsWhenTheAccountIsLoggedInAgain(t *testing.T) {
	isolateEngine(t)
	a := seedAccount(t, "u-1", "org-1")
	e := engineFor(t, tokensAreFine, noFetch)
	spendStoredLogin(t, e, "u-1")
	// `ccdad add` stores a brand-new grant.
	storeLogin(t, a, blobExpiring("RT-u-1-relogin", tickEpoch.Add(10*time.Hour)))
	passSees(t, e)
	if err := e.holdSpentToken("u-1"); err != nil {
		t.Fatalf("the hold stayed after a re-login replaced the spent login: %v", err)
	}
}

// storeLogin stores login as a's, as a later adoption or a re-login does.
func storeLogin(t *testing.T, a store.Account, login string) {
	t.Helper()
	var b cclink.Blob
	mustUnmarshal(t, login, &b)
	if err := openEngineStore(t).Add(a, b); err != nil {
		t.Fatal(err)
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
	spendStoredLogin(t, e, "u-2")

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
	seedAccount(t, "u-1", "org-1")
	e := NewEngine()
	e.Now = func() time.Time { return tickEpoch }
	a := store.Account{UUID: "u-1", Provider: provider.Claude, SubscriptionStatus: "active"}
	entry := usage.Entry{Snapshot: unusedWindow(), FetchedAt: tickEpoch.Add(-10 * time.Minute)}
	cfg := config.Defaults()
	thresholds := configuredThresholds(cfg)
	if _, _, want := e.probeDue(a, entry, cfg, thresholds, tickEpoch, "", true, nil); !want {
		t.Fatal("precondition: the probe is not due without a hold")
	}
	spendStoredLogin(t, e, "u-1")
	if _, _, want := e.probeDue(a, entry, cfg, thresholds, tickEpoch, "", true, nil); want {
		t.Fatal("a probe is due for an account whose stored refresh token is spent")
	}
}
