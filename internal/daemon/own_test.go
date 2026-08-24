package daemon

import (
	"context"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// seedElsewhere stores an account and hands it to another machine's ccdad.
func seedElsewhere(t *testing.T, uuid, org string) {
	t.Helper()
	seedAccount(t, uuid, org)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	// Everything NOT named goes to the other machine, so the owned set is every
	// account already stored minus this one.
	var owned []string
	for _, a := range s.Accounts() {
		if a.UUID != uuid && !a.Elsewhere {
			owned = append(owned, a.UUID)
		}
	}
	if len(owned) == 0 {
		t.Fatal("seedElsewhere needs an account to stay on this machine")
	}
	if _, err := s.SetOwned(owned); err != nil {
		t.Fatal(err)
	}
}

// An account another machine drives is not polled on a cadence. The reading
// spends a budget shared with whoever IS driving it, and this machine can never
// rank it, so the request buys nothing at all.
func TestAnAccountAnotherMachineOwnsIsNotPolled(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedElsewhere(t, "u-2", "org-2")
	liveAs(t, "u-1")

	polled := map[string]bool{}
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		polled[token] = true
		return snapshotWith(20), nil
	})
	tick(t, e)

	if !polled["AT-u-1"] {
		t.Error("this machine's own account was not polled")
	}
	if polled["AT-u-2"] {
		t.Error("an account another machine drives was polled, spending a budget shared " +
			"with the machine that is actually using it")
	}
	if _, ok := cacheEntry(t, "u-2"); ok {
		t.Error("a reading was cached for an account this machine does not drive")
	}
}

// The live account is the exception, and it is not a courtesy.
//
// The hysteresis baseline, the threshold test and the pre-emption projection are
// all statements about the account Claude Code is logged in as. Going blind on
// that one makes every tick the no-baseline case, which the anti-flap cooldown is
// deliberately exempt from -- so the machine would name a target every tick.
func TestTheLiveAccountIsPolledEvenWhenAnotherMachineOwnsIt(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedElsewhere(t, "u-2", "org-2")
	// The account this machine does NOT own is the one Claude Code is on, which
	// is the ordinary state right after a split is declared.
	liveAs(t, "u-2")

	polled := map[string]bool{}
	e := engineFor(t, tokensAreFine, func(_ context.Context, token string) (*usage.Snapshot, error) {
		polled[token] = true
		return snapshotWith(20), nil
	})
	tick(t, e)

	if !polled["AT-u-2"] {
		t.Error("the live account went unread because another machine owns it, which " +
			"leaves the engine with no baseline for the one account a session runs on")
	}
	if _, ok := cacheEntry(t, "u-2"); !ok {
		t.Error("no reading was cached for the live account")
	}
}
