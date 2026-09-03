package daemon

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func codexRow(uuid string) store.Account {
	return store.Account{UUID: uuid, Email: uuid + "@example.com", Provider: provider.Codex}
}

func emptyCache(t *testing.T) *usage.Cache {
	t.Helper()
	c, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The document is the only place on the machine that records what the lane
// decided, and `ccdad status`, the dashboard and doctor all read it from here.
func TestPublishCarriesTheCodexServingAccountAndItsStamp(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	e.Now = func() time.Time { return tickEpoch }
	a := codexRow("cx-1")

	e.publish([]store.Account{a}, emptyCache(t),
		switcher.Evaluation{},
		switcher.Evaluation{
			Live: a, LiveKnown: true,
			LastSwitchAt: tickEpoch, LastSwitchTo: "cx-1",
		},
		configuredThresholds(config.Defaults()), map[string]bool{})

	got := e.Snapshot()
	if got.CodexServingUUID != "cx-1" {
		t.Fatalf("CodexServingUUID = %q, want cx-1", got.CodexServingUUID)
	}
	if !got.CodexLastSwitchAt.Equal(tickEpoch) || got.CodexLastSwitchTo != "cx-1" {
		t.Fatalf("codex stamp = (%v, %q), want (%v, cx-1)",
			got.CodexLastSwitchAt, got.CodexLastSwitchTo, tickEpoch)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].State != StateServing {
		t.Fatalf("state = %v, want %v", got.Accounts, StateServing)
	}
}

// The Claude half of the same document must not move when the Codex half does:
// activeUuid is Claude's and nothing here may set it.
func TestACodexEvaluationNeverSetsTheActiveAccount(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	e.Now = func() time.Time { return tickEpoch }
	a := codexRow("cx-1")

	e.publish([]store.Account{a}, emptyCache(t),
		switcher.Evaluation{},
		switcher.Evaluation{Live: a, LiveKnown: true},
		configuredThresholds(config.Defaults()), map[string]bool{})

	if got := e.Snapshot().ActiveUUID; got != "" {
		t.Fatalf("ActiveUUID = %q; the codex pointer is not the Claude login", got)
	}
}

// A dead grant is the one account fact whose remedy is a command, so it outranks
// "this one is currently serving": a user reading `serving` beside an account
// that answers 401 on every turn has been told the wrong thing.
func TestAnAccountWithADeadGrantPublishesNeedsRelogin(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	e.Now = func() time.Time { return tickEpoch }
	a := codexRow("cx-1")
	e.codexRelogin = map[string]bool{"cx-1": true}

	e.publish([]store.Account{a}, emptyCache(t),
		switcher.Evaluation{},
		switcher.Evaluation{Live: a, LiveKnown: true},
		configuredThresholds(config.Defaults()), map[string]bool{})

	if got := e.Snapshot().Accounts[0].State; got != StateNeedsRelogin {
		t.Fatalf("state = %v, want %v", got, StateNeedsRelogin)
	}
}

// Never-cross: the daemon spawns `probe --uuid --force` children, and a probe
// runs a real Claude Code out of a credential home seeded from the account's
// stored login. There is no such login on a Codex account, and there is no
// codex-flavoured probe at all.
//
// The fixture matters here in a way it does not for a Claude-account test of
// the same gate: strategy.ColdWindow only returns ok=true for a window that
// has never been spent (no reset reported at all) or one whose reset has
// already passed. snapshotWith always attaches a reset an hour in the
// FUTURE, so a fixture built from it never reaches that ok=true branch, and
// probeDue's pre-existing `if !ok { return "", "", false }` would refuse the
// probe before the provider gate below it is ever evaluated -- which would
// let this test pass even if that gate were deleted outright. The window
// here is built directly with a reset AT tickEpoch instead, so ColdWindow's
// rollover arm is genuinely satisfied, MayProbe has no prior attempt to
// object with, and the only thing standing between this fixture and a
// probe is the provider check.
func TestTheProbeIsRefusedForEveryCodexAccount(t *testing.T) {
	isolateEngine(t)
	e := NewEngine()
	e.Now = func() time.Time { return tickEpoch }

	cfg := config.Defaults()
	cfg.ProbeUnknown = true
	a := codexRow("cx-1")
	used := 10.0
	resets := tickEpoch
	entry := usage.Entry{
		Snapshot:  &usage.Snapshot{FiveHour: usage.NewWindow(&used, &resets)},
		FetchedAt: tickEpoch.Add(-time.Hour),
	}

	_, _, want := e.probeDue(a, entry, cfg, configuredThresholds(cfg),
		tickEpoch, "cl-1", true, map[string]bool{})
	if want {
		t.Fatal("probeDue wants to probe a codex account")
	}
}
