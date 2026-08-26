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
	if got := accountState(a, cache, false, "", configuredThresholds(bare)); got != StateCandidate {
		t.Fatalf("accountState() with no table = %q, want %q — 70%% used is under the default threshold", got, StateCandidate)
	}

	tuned := config.Config{
		Threshold:       80,
		WindowThreshold: map[usage.WindowName]float64{usage.WindowSevenDay: 60},
	}
	if got := accountState(a, cache, false, "", configuredThresholds(tuned)); got != StateExhausted {
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
		e.Rand = midJitter
		e.commit(a, perWindowSnapshot(), tickEpoch, []string{a.UUID}, configuredThresholds(cfg), true, nil)
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

// creditOnlySnapshot is a seat metered in money and nothing else: no plan
// window carried a figure, and extra_usage holds the whole meter. The figures
// are a live claude_enterprise seat read on 2026-08-26, in wire minor units.
func creditOnlySnapshot(utilization float64) *usage.Snapshot {
	u, limit, used := utilization, 200000.0, 120451.0
	return &usage.Snapshot{ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
		State:        usage.ExtraUsageEnabled,
		Currency:     "USD",
		MonthlyLimit: &limit,
		UsedCredits:  &used,
		Utilization:  &u,
	})}
}

// TestASeatMeteredOnlyInMoneyPublishesARealState is what a fleet of enterprise
// seats needs before anything downstream of status.json can work.
//
// accountState measured the window-only axis, so every such seat published
// StateUnknown no matter how much of its balance was gone — and Unknown is the
// answer that means "nobody could read this", which is exactly wrong for an
// account whose meter was read perfectly well. Everything keyed on the states
// below is inert for the whole fleet until this reads the right axis.
func TestASeatMeteredOnlyInMoneyPublishesARealState(t *testing.T) {
	cfg := config.Config{Threshold: 80, CreditThreshold: 80}
	cases := []struct {
		name        string
		utilization float64
		want        AccountState
	}{
		{"most of the balance left", 60.2255, StateCandidate},
		{"past the credit threshold", 90, StateExhausted},
		{"the balance is gone", 100, StateEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := &usage.Cache{}
			cache.Put("acct-1", usage.Entry{Snapshot: creditOnlySnapshot(tc.utilization), FetchedAt: tickEpoch})
			a := store.Account{UUID: "acct-1", Primary: true}
			if got := accountState(a, cache, false, "", configuredThresholds(cfg)); got != tc.want {
				t.Errorf("accountState() = %q, want %q", got, tc.want)
			}
		})
	}
}
