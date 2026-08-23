package switcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// seedReading puts a usage reading in the on-disk cache.
func seedReading(t *testing.T, uuid string, headroom float64) {
	t.Helper()
	pct := 100 - headroom
	resets := time.Now().Add(time.Hour)
	snap := &usage.Snapshot{FiveHour: usage.NewWindow(&pct, &resets)}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(uuid, usage.Entry{Snapshot: snap, FetchedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// seedBurning caches a reading three hours into a five-hour window — 1472
// seconds from the limit at 88% — together with the poll interval the scheduler
// would have written beside it.
func seedBurning(t *testing.T, uuid string, pct float64, now time.Time, interval time.Duration) {
	t.Helper()
	resets := now.Add(2 * time.Hour)
	snap := &usage.Snapshot{FiveHour: usage.NewWindow(&pct, &resets)}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(uuid, usage.Entry{Snapshot: snap, FetchedAt: now, NextPollAt: now.Add(interval)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The pre-emptive switch reads two stamps the SCHEDULER wrote — when a reading
// was taken and when the next one is due — so they have to survive the trip from
// the usage cache into the engine. Nothing else in the ranking reads either, so
// dropping them here is invisible in every other test and shows up only as an
// account cut off mid-session.
//
// It also pins the other half of the seam: no strategy option is set here, so
// the 2 minute lead this depends on has to arrive from the config defaults
// through RankOptions. A lead that stopped being carried would land as a stay.
func TestEvaluateCarriesThePollStampsIntoTheEngine(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")

	// After the seeds, so neither reading is older than its account's AddedAt
	// and Prune keeps both.
	now := time.Now().UTC()
	// u-1 is 1472 seconds from its limit and the next poll is 1800 seconds out —
	// the ceiling a 429 imposes — so the engine is blind straight past it. u-2 is
	// one point under its threshold, which clears neither ordinary margin.
	seedBurning(t, "u-1", 88, now, 1800*time.Second)
	seedBurning(t, "u-2", 79, now, 1800*time.Second)

	ev, err := Evaluate(openStore(t), EvalOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Plan.Action != strategy.ActionSwitch || ev.Plan.Reason != strategy.ReasonProjectedExhaustion {
		t.Fatalf("Action = %v, Reason = %v; want a switch on the projection — the stamps did not reach the ranking",
			ev.Plan.Action, ev.Plan.Reason)
	}
	if !ev.HasTarget || ev.Target.UUID != "u-2" {
		t.Fatalf("Target = %q (%v), want u-2", ev.Target.UUID, ev.HasTarget)
	}
}

func writeConfigFile(t *testing.T, body string) {
	t.Helper()
	path := mustPath(config.Path())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The last-good-config rule says an unusable config leaves the engine on the
// LAST CONFIG THAT PARSED, with a warning — and falling back to the built-in
// defaults is not that. It is the right answer for a one-shot, which has no
// previous config to keep, and the wrong one for the daemon, whose thresholds
// would silently jump back to stock the moment somebody mistyped an edit.
//
// So the source is a parameter. Supplied, it is authoritative and the file on
// disk is not read at all.
func TestEvaluateTakesItsConfigFromTheSuppliedSource(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	seedReading(t, "u-1", 10)
	seedReading(t, "u-2", 80)
	// A file that cannot be parsed at all. If Evaluate reads it, ConfigErr says
	// so and the threshold below is not the one in force.
	writeConfigFile(t, "threshold = = 90\n")

	supplied := config.Defaults()
	supplied.Strategy = strategy.StrategyConsumeFirst
	ev, err := Evaluate(openStore(t), EvalOptions{
		Config: func() (config.Config, error) { return supplied, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ConfigErr != nil {
		t.Fatalf("ConfigErr = %v, want the supplied source to be authoritative", ev.ConfigErr)
	}
	if got := ev.Plan.Result.Mode; got != strategy.ModeConsumeFirst {
		t.Fatalf("mode = %v, want the supplied strategy to be in force", got)
	}
}

// A source that reports a problem still supplies a config — that is the
// Reloader's contract, and it is what "with a warning" means. The warning
// reaches the caller; the config it came with is used.
func TestASourceWarningDoesNotDiscardTheConfigItCameWith(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	seedReading(t, "u-1", 10)
	seedReading(t, "u-2", 80)

	warn := errors.New("threshold = = 90: expected value")
	kept := config.Defaults()
	kept.Strategy = strategy.StrategyConsumeFirst
	ev, err := Evaluate(openStore(t), EvalOptions{
		Config: func() (config.Config, error) { return kept, warn },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ev.ConfigErr, warn) {
		t.Fatalf("ConfigErr = %v, want the source's warning", ev.ConfigErr)
	}
	if got := ev.Plan.Result.Mode; got != strategy.ModeConsumeFirst {
		t.Fatalf("mode = %v, want the config the warning came with, not the defaults", got)
	}
}

// With no source the one-shot behaviour stands: read the file, and fall back to
// the documented defaults rather than refusing to switch over a typo.
func TestWithNoSourceAnUnusableFileFallsBackToTheDefaults(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	seedReading(t, "u-1", 10)
	seedReading(t, "u-2", 80)
	writeConfigFile(t, "threshold = = 90\n")

	ev, err := Evaluate(openStore(t), EvalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ev.ConfigErr == nil {
		t.Fatal("ConfigErr = nil, want the parse failure reported")
	}
	if got := ev.Plan.Result.Mode; got != strategy.ModeHeadroom {
		t.Fatalf("mode = %v, want the built-in default", got)
	}
	if !ev.HasTarget || ev.Target.UUID != "u-2" {
		t.Fatalf("target = %+v, want the engine still to have chosen", ev.Target)
	}
}

// Candidate.Primary is what tells the engine that a credit-metered seat may be
// ranked with the accounts whose quota is already paid for. The flag lives on
// store.Account and the engine sees only a strategy.Candidate, so a projection
// that drops it leaves the entire primary path dead in the shipped binary while
// every unit test in internal/strategy still passes -- those build their
// candidates by hand and never go through here.
func TestTheProjectionCarriesThePrimaryFlag(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	seed(t, "u-2", "two@example.com")

	s := openStore(t)
	if _, err := s.SetPrimary("u-2", true); err != nil {
		t.Fatal(err)
	}

	cache, err := usage.LoadCache()
	if err != nil {
		t.Fatal(err)
	}
	cands := engineCandidates(s, s.Accounts(), cache)
	if len(cands) != 2 {
		t.Fatalf("engineCandidates() returned %d candidates, want both accounts", len(cands))
	}
	for _, c := range cands {
		if want := c.UUID == "u-2"; c.Primary != want {
			t.Errorf("%s: Primary = %v, want %v", c.UUID, c.Primary, want)
		}
	}
}
