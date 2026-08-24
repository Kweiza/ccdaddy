package daemon

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// perWindowSnapshot is an account whose five-hour window is comfortable and
// whose seven-day window is at 70%: spent under a per-window threshold of 60,
// healthy under the default of 80.
func perWindowSnapshot() *usage.Snapshot {
	five, seven := 10.0, 70.0
	at := tickEpoch.Add(time.Hour)
	return &usage.Snapshot{
		FiveHour: usage.NewWindow(&five, &at),
		SevenDay: usage.NewWindow(&seven, &at),
	}
}

// The daemon's own headroom pass has to read the SAME thresholds the ranking
// does. It used to build strategy.Thresholds{Default: cfg.Threshold} by hand,
// which silently dropped the per-window table — so an account the engine refuses
// showed as a candidate in `ccdad status`, and, worse once an unnamed scope can
// be opted in, the two passes disagreed about which windows exist at all rather
// than only about a number.
//
// config.Config.Thresholds() is the one bundle. This is what holds the daemon to
// it, because a hand-built literal compiles perfectly and answers differently.
func TestTheDaemonMeasuresAgainstThePerWindowTable(t *testing.T) {
	cache := &usage.Cache{}
	cache.Put("acct-1", usage.Entry{Snapshot: perWindowSnapshot(), FetchedAt: tickEpoch})
	a := store.Account{UUID: "acct-1"}

	bare := config.Config{Threshold: 80}
	if got := accountState(a, cache, false, "", bare); got != StateCandidate {
		t.Fatalf("accountState() with no table = %q, want %q — 70%% used is under the default threshold", got, StateCandidate)
	}

	tuned := config.Config{
		Threshold:       80,
		WindowThreshold: map[usage.WindowName]float64{usage.WindowSevenDay: 60},
	}
	if got := accountState(a, cache, false, "", tuned); got != StateExhausted {
		t.Errorf("accountState() with seven_day capped at 60 = %q, want %q — the daemon must measure each window against the threshold the ranking measures it against", got, StateExhausted)
	}
}

// The poll cadence is the OTHER reader of the same headroom, and it is the one
// with nobody watching: a status column that reads wrong gets noticed, a poll
// interval that reads wrong just quietly spends the wrong budget. It is measured
// through commit rather than through the helper, because the defect this closes
// was a Thresholds value built by hand AT THE CALL SITE — a test of the helper
// alone cannot see one.
func TestTheDaemonPollCadenceMeasuresAgainstThePerWindowTable(t *testing.T) {
	isolateEngine(t)
	a := store.Account{UUID: "acct-1"}

	// The account is ACTIVE on purpose. CandidateMaxInterval and
	// ExhaustedInterval are both ten minutes, so an idle account gets the same
	// cadence whether the table reached the pass or not — the two answers would
	// be identical for opposite reasons and the test could not fail. Active
	// splits them: five minutes when it is not spent, ten when it is.
	nextPollAfter := func(cfg config.Config) time.Time {
		t.Helper()
		e := NewEngine()
		e.commit(a, perWindowSnapshot(), tickEpoch, 1, cfg, true, nil)
		var at time.Time
		if err := usage.WithCache(cacheTimeout, func(c *usage.Cache) error {
			entry, ok := c.Get(a.UUID)
			if !ok {
				t.Fatal("commit() wrote no cache entry")
			}
			at = entry.NextPollAt
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return at
	}

	bare := nextPollAfter(config.Config{Threshold: 80})
	tuned := nextPollAfter(config.Config{
		Threshold:       80,
		WindowThreshold: map[usage.WindowName]float64{usage.WindowSevenDay: 60},
	})

	if want := tickEpoch.Add(5 * time.Minute); !bare.Equal(want) {
		t.Errorf("with no table the next poll is %v, want %v — an unspent active account polls at the active cadence", bare, want)
	}
	if want := tickEpoch.Add(10 * time.Minute); !tuned.Equal(want) {
		t.Errorf("with seven_day capped at 60 the next poll is %v, want %v — the table never reached the cadence, so an account the engine calls spent is still polled as a healthy one", tuned, want)
	}
}
