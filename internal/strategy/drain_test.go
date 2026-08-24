package strategy

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The drain tests. They ask the one question a table of thresholds cannot
// answer on its own: over a whole week, does the engine actually SPEND the pool,
// or does it stop moving with quota still in it?
//
// Each drives Decide in a loop against a pool that burns down as it goes, and
// asserts that nothing is left at the end. Before OutOfQuota existed the answer
// was no in all three: the fixed-threshold run stranded a hysteresis margin in
// every account, and both hover runs parked on the first account to reach 100%
// and refused every move off it, because slack saturates at -1 there and no
// candidate can clear a five-point margin against it.

// simAccount is one account's weekly window over the run.
type simAccount struct {
	uuid    string
	util    float64
	resetIn time.Duration
}

// runSim drives Decide in a loop: decide, switch if told to, burn quota on
// whichever account is live, advance the clock. It answers the only question
// that matters about the tail -- how much quota is left unspent when the engine
// stops moving.
func runSim(t *testing.T, label string, accts []simAccount, hover bool, burnPerTick float64, ticks int, cfg Config) float64 {
	t.Helper()
	base := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	const tick = 2 * time.Minute

	st := NewState()
	live := accts[0].uuid
	switches := 0
	reasons := map[string]int{}
	stuckAt := -1

	for i := 0; i < ticks; i++ {
		now := base.Add(time.Duration(i) * tick)
		cands := make([]Candidate, 0, len(accts))
		for _, a := range accts {
			reset := now.Add(a.resetIn - time.Duration(i)*tick)
			pct := a.util
			five := usage.NewWindow(fp(0), tp(now.Add(3*time.Hour)))
			seven := usage.NewWindow(&pct, &reset)
			c := sub(a.uuid, &usage.Snapshot{FiveHour: five, SevenDay: seven})
			c.FetchedAt = now.Add(-time.Minute)
			c.NextPollAt = now.Add(time.Minute)
			cands = append(cands, c)
		}
		o := Options{Now: now, Hover: hover}
		if !hover {
			o.Threshold = 80
		}
		p := Decide(cands, o, cfg, st, live)
		reasons[p.Action.String()+": "+p.Reason.String()]++
		if p.Action == ActionSwitch {
			live = p.Target.UUID
			st.RecordSwitch(live, now)
			switches++
		}
		// Burn on the live account, if it has anything left.
		for j := range accts {
			if accts[j].uuid != live {
				continue
			}
			if accts[j].util >= 100 {
				if stuckAt < 0 {
					stuckAt = i
				}
				break
			}
			accts[j].util += burnPerTick
			if accts[j].util > 100 {
				accts[j].util = 100
			}
		}
	}

	total, unused := 0.0, 0.0
	parts := make([]string, 0, len(accts))
	for _, a := range accts {
		total += 100
		unused += 100 - a.util
		parts = append(parts, fmt.Sprintf("%s=%.1f%%", a.uuid, a.util))
	}
	t.Logf("%s", label)
	t.Logf("   final utilization: %v", parts)
	t.Logf("   live=%s switches=%d", live, switches)
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("   %5d ticks  %s", reasons[k], k)
	}
	t.Logf("   UNUSED QUOTA: %.1f of %.0f points (%.1f%% of the pool wasted)", unused, total, unused/total*100)
	if stuckAt >= 0 {
		t.Logf("   engine was sitting on a 100%%-spent account from tick %d onward", stuckAt)
	}
	return unused
}

func fp(v float64) *float64     { return &v }
func tp(t time.Time) *time.Time { return &t }

// Q1: fixed-threshold strategy. Every account over 80. Does the engine keep
// rotating until all six are actually at 100?
func TestEveryAccountDrainsUnderAFixedThreshold(t *testing.T) {
	accts := []simAccount{
		{"a", 82, 5 * 24 * time.Hour},
		{"b", 84, 5 * 24 * time.Hour},
		{"c", 86, 5 * 24 * time.Hour},
		{"d", 88, 5 * 24 * time.Hour},
		{"e", 90, 5 * 24 * time.Hour},
		{"f", 92, 5 * 24 * time.Hour},
	}
	unused := runSim(t, "fixed threshold 80, defaults (hysteresis 10, ratio 2, cooldown 5m)", accts, false, 0.5, 900, Config{})
	if unused > 0 {
		t.Errorf("%.1f points left unspent: the engine stopped rotating before the pool was drained", unused)
	}
}

// Q2: hover. Every account already past the 99 cap. Can the last point of each
// be spent?
func TestTheLastPointOfEachAccountIsReachableUnderHover(t *testing.T) {
	accts := []simAccount{
		{"a", 99.0, 4 * time.Hour},
		{"b", 99.1, 5 * time.Hour},
		{"c", 99.2, 6 * time.Hour},
		{"d", 99.3, 7 * time.Hour},
		{"e", 99.4, 8 * time.Hour},
		{"f", 99.5, 9 * time.Hour},
	}
	unused := runSim(t, "hover, every account past the 99 cap", accts, true, 0.05, 900, Config{})
	if unused > 0 {
		t.Errorf("%.1f points left unspent: the cap at HoverCap put every slack inside one point of "+
			"every other, so no candidate could clear the margin and the tail was unreachable", unused)
	}
}

// Q2b: hover from a realistic mid-week spread, run long.
func TestHoverDrainsTheWholePoolFromAMidWeekSpread(t *testing.T) {
	accts := []simAccount{
		{"a", 100, 17 * time.Hour},
		{"b", 80, 4*24*time.Hour + 21*time.Hour},
		{"c", 53, 5*24*time.Hour + 23*time.Hour},
		{"d", 70, 4*24*time.Hour + 12*time.Hour},
		{"e", 79, 3*24*time.Hour + 9*time.Hour},
		{"f", 80, 4*24*time.Hour + 14*time.Hour},
	}
	unused := runSim(t, "hover, the live pool of 2026-08-24", accts, true, 0.5, 900, Config{})
	if unused > 0 {
		t.Errorf("%.1f of 600 points left unspent", unused)
	}
}
