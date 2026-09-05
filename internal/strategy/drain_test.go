package strategy

import (
	"fmt"
	"math"
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

// Q3: the pace invariant itself. Draining the pool is one property; holding
// every account NEAR ITS OWN PACE LINE while doing it is the other, and it is
// the one hover's whole threshold formula exists to produce.
//
// Uncapped, threshold = ExpectedPct + share and share is one number for the
// whole pool, so slack = share + (ExpectedPct - util) and ordering on slack IS
// ordering on the pace deficit, exactly. The clamp used to break that identity
// for whichever account was furthest through its own window -- above the clamp
// the elapsed term was thrown away and the pool was ordered on raw utilization,
// which is precisely the wrong axis for the account whose quota expires soonest.
//
// Measured in this harness with the clamp restored: final-day spread mean 20.14,
// max 24.23, on 12 switches. Without it: mean 9.89, max 14.43, on 9 switches --
// half the error for fewer moves. The bound below sits between the two, so
// restoring any clamp on the derived threshold turns this red.
//
// The share gained a second half after that was measured, and the numbers here
// are unchanged to the digit -- which is the property rather than a
// coincidence, and worth naming so a later reader does not take this green as
// coverage of it. The accounts in this pool strand 7.14 points against a pool
// share of 33.33, so hoverShare's maximum returns the flat slice on every tick
// and the pace identity above is untouched. The case that actually exercises
// the other half is TestHoverSpendsAWeekThatWouldOtherwiseExpireUnspent.
func TestHoverHoldsEveryAccountNearItsOwnPaceLine(t *testing.T) {
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	const week = 7 * 24 * time.Hour
	const tick = 30 * time.Minute
	const ticks = 240
	const burn = 0.35

	type acct struct {
		uuid  string
		util  float64
		reset time.Time
	}
	// Resets one, three and five days out: elapsed shares of 85.7, 57.1 and 28.6
	// against one starting utilization, which is the spread the engine has to
	// close.
	as := []*acct{
		{"a1", 50, base.Add(1 * 24 * time.Hour)},
		{"a2", 50, base.Add(3 * 24 * time.Hour)},
		{"a3", 50, base.Add(5 * 24 * time.Hour)},
	}

	st, live, switches := NewState(), "a1", 0
	var dayMax, daySum float64
	var dayN int

	for i := 0; i < ticks; i++ {
		now := base.Add(time.Duration(i) * tick)
		cands := make([]Candidate, 0, len(as))
		for _, a := range as {
			if !a.reset.After(now) {
				a.reset, a.util = a.reset.Add(week), 0
			}
			pct, reset := a.util, a.reset
			c := sub(a.uuid, &usage.Snapshot{SevenDay: usage.NewWindow(&pct, &reset)})
			c.FetchedAt, c.NextPollAt = now.Add(-time.Minute), now.Add(time.Minute)
			cands = append(cands, c)
		}
		if p := Decide(cands, Options{Now: now, Hover: true}, Config{}, st, live); p.Action == ActionSwitch {
			live, switches = p.Target.UUID, switches+1
			st.RecordSwitch(live, now)
		}
		for _, a := range as {
			if a.uuid == live {
				a.util = math.Min(100, a.util+burn)
			}
		}
		// The FINAL DAY only. The run-wide maximum is dominated by the initial
		// condition -- three accounts at one utilization while their elapsed
		// shares are 85.7/57.1/28.6 is a spread of 57 before the engine has
		// decided anything -- so a bound on it would measure the fixture.
		if i < ticks-48 {
			continue
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, a := range as {
			pct, reset := a.util, a.reset
			expected, _ := usage.ExpectedPct(usage.WindowSevenDay, usage.NewWindow(&pct, &reset), now)
			d := a.util - expected
			lo, hi = math.Min(lo, d), math.Max(hi, d)
		}
		dayMax, daySum, dayN = math.Max(dayMax, hi-lo), daySum+(hi-lo), dayN+1
	}

	mean := daySum / float64(dayN)
	t.Logf("   final-day spread of (util - expected): max %.2f, mean %.2f, on %d switches",
		dayMax, mean, switches)
	if mean > 15 {
		t.Errorf("final-day spread mean = %.2f, want under 15: the pool is no longer being held "+
			"to its own pace lines (with the HoverCap clamp restored this reads ~20)", mean)
	}
}

// runPaceSim drives Decide over n accounts whose weekly windows reset at evenly
// spread points across one week, working the fleet at `load` of its own capacity,
// and reports the final day's mean spread of (util - expected), the switch count
// and how many ticks ran in recovery mode.
//
// The load is scaled by n deliberately. A fixed workload spread over a bigger
// fleet leaves most of it idle, and idle accounts fall behind their pace lines
// for want of work rather than for want of scheduling -- which would measure the
// fixture and call it a defect.
func runPaceSim(t *testing.T, n int, load float64) (float64, int, int) {
	t.Helper()
	base := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	const week = 7 * 24 * time.Hour
	const tick = 30 * time.Minute
	const ticks = 240

	type acct struct {
		uuid  string
		util  float64
		reset time.Time
	}
	as := make([]*acct, 0, n)
	for i := 0; i < n; i++ {
		off := time.Duration(float64(i+1) / float64(n) * float64(week))
		as = append(as, &acct{uuid: fmt.Sprintf("p-%02d", i), util: 50, reset: base.Add(off)})
	}
	burn := load * float64(n) * 100.0 / (float64(week) / float64(tick))

	st, live, switches := NewState(), as[0].uuid, 0
	recovery, dayN := 0, 0
	var daySum float64

	for i := 0; i < ticks; i++ {
		now := base.Add(time.Duration(i) * tick)
		cands := make([]Candidate, 0, n)
		for _, a := range as {
			if !a.reset.After(now) {
				a.reset, a.util = a.reset.Add(week), 0
			}
			pct, reset := a.util, a.reset
			c := sub(a.uuid, &usage.Snapshot{SevenDay: usage.NewWindow(&pct, &reset)})
			c.FetchedAt, c.NextPollAt = now.Add(-time.Minute), now.Add(time.Minute)
			cands = append(cands, c)
		}
		p := Decide(cands, Options{Now: now, Hover: true}, Config{}, st, live)
		if p.Result.Mode == ModeRecovery {
			recovery++
		}
		if p.Action == ActionSwitch {
			live, switches = p.Target.UUID, switches+1
			st.RecordSwitch(live, now)
		}
		for _, a := range as {
			if a.uuid == live {
				a.util = math.Min(100, a.util+burn)
			}
		}
		if i < ticks-48 {
			continue
		}
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, a := range as {
			pct, reset := a.util, a.reset
			e, _ := usage.ExpectedPct(usage.WindowSevenDay, usage.NewWindow(&pct, &reset), now)
			d := a.util - e
			lo, hi = math.Min(lo, d), math.Max(hi, d)
		}
		daySum, dayN = daySum+(hi-lo), dayN+1
	}
	return daySum / float64(dayN), switches, recovery
}

// Q4: does the pace invariant survive a fleet of a different size? A pool is two
// accounts for one person and fifty seats for a team, and share = 100/N is the
// one place N enters the formula.
//
// The POOL half of the share does not enter the ORDERING: 100/N is added
// identically to every account, so it cancels from every comparison the ranking
// makes. Now that nothing clamps the derived threshold, that cancellation is
// exact rather than approximate -- which is the property this case exists to
// keep. What N does reach is the Spent boundary (util > expected + 100/N), and
// through it allOver and ModeRecovery.
//
// The STRANDED half does not cancel, and that is a real difference this case is
// deliberately blind to. It is per account, so adding or quarantining an account
// can now reorder a pool rather than only retune it -- N appears in the stranded
// arithmetic too, and an account whose week the rotation could reach with four
// peers may strand quota with two. The pool here works at 95% of its own
// capacity, so utilization tracks pace and every account strands zero at every N
// below; that is what makes the measured means comparable across sizes, and it
// is why this case measures the cancellation that survives rather than the one
// that no longer holds in general.
//
// Measured, final-day spread of (util - expected) across the pool, fleet worked
// at 95% of its own capacity, weekly windows only:
//
//	N= 2  mean 17.25   N= 3  mean 14.08   N= 6  mean  4.00
//	N=10  mean  4.78   N=50  mean 13.84
//
// Two accounts is worse than six because two accounts have nowhere to put the
// correction, not because the engine is worse at it. Fifty is worse for the
// opposite reason: the pool's natural granularity falls to about two points,
// which is under HoverHysteresisPct, so the lead changes hands almost every
// evaluation -- 239 of 240 ticks here. In production HoverCooldown bounds that
// rate; at this harness's thirty-minute tick it never binds. A fleet that large
// wanting quieter switching should raise the margin, and pay for it in tolerance
// point for point.
func TestThePaceInvariantHoldsAcrossFleetSizes(t *testing.T) {
	for _, n := range []int{2, 3, 6, 10, 50} {
		mean, switches, recovery := runPaceSim(t, n, 0.95)
		t.Logf("   N=%2d  share=%5.2f  final-day spread mean %6.2f  switches %3d  recovery %3d/240",
			n, 100/float64(n), mean, switches, recovery)
		// The bound is the loose one on purpose: this pins that no pool size
		// falls apart, not a figure per size. The interesting sizes are pinned
		// tightly by TestHoverHoldsEveryAccountNearItsOwnPaceLine.
		if mean > 35 {
			t.Errorf("N=%d: final-day spread mean %.2f, want under 35", n, mean)
		}
	}
}

// Q5: does the engine spend a week that is about to expire, or throw it away?
//
// Every case above drives a pool whose accounts all reset together, which is the
// one shape in which the question cannot arise. A real fleet is staggered --
// accounts are added on different days and their weeks end on different days
// ever after -- and then the pool holds quota with a deadline on it. What this
// measures is the only thing that matters about that: how many weekly points
// reach their reset unspent.
//
// The five-hour window is modelled at its true relation to the week rather than
// as a second free axis. A week is 33.6 times longer than a five-hour window, so
// the same turn of work climbs the five-hour window 33.6 times faster; that is
// what forces the rotation, and it is why an account cannot simply be left live
// until its week is gone.
type wasteAccount struct {
	uuid string
	// weekUtil and fiveUtil are the two windows, and weekAt and fiveAt when each
	// next rolls over.
	weekUtil, fiveUtil float64
	weekAt, fiveAt     time.Time
	// served is how much weekly quota this account actually absorbed, and
	// wasted how much reached a reset unspent.
	served, wasted float64
}

// fiveHoursPerWeek is how many five-hour windows fit in a week, which is the
// factor the same work climbs the shorter window by.
const fiveHoursPerWeek = float64(7*24*time.Hour) / float64(5*time.Hour)

// runWasteSim drives Decide over a staggered fleet for a day and returns the
// weekly points thrown away at rollovers, plus what each account absorbed.
func runWasteSim(t *testing.T, label string, accts []wasteAccount, hover bool, weeklyPerTick float64, ticks int) ([]wasteAccount, float64) {
	t.Helper()
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	const tick = 2 * time.Minute
	const week = 7 * 24 * time.Hour
	const five = 5 * time.Hour

	st := NewState()
	live := accts[0].uuid
	switches := 0

	for i := 0; i < ticks; i++ {
		now := base.Add(time.Duration(i) * tick)

		for j := range accts {
			// A weekly rollover is where the accounting happens: whatever the
			// window still held is gone, and no later tick can spend it.
			if !now.Before(accts[j].weekAt) {
				accts[j].wasted += 100 - accts[j].weekUtil
				accts[j].weekUtil, accts[j].weekAt = 0, accts[j].weekAt.Add(week)
			}
			if !now.Before(accts[j].fiveAt) {
				accts[j].fiveUtil, accts[j].fiveAt = 0, accts[j].fiveAt.Add(five)
			}
		}

		cands := make([]Candidate, 0, len(accts))
		for _, a := range accts {
			fiveUtil, weekUtil := a.fiveUtil, a.weekUtil
			c := sub(a.uuid, &usage.Snapshot{
				FiveHour: usage.NewWindow(&fiveUtil, tp(a.fiveAt)),
				SevenDay: usage.NewWindow(&weekUtil, tp(a.weekAt)),
			})
			c.FetchedAt, c.NextPollAt = now.Add(-time.Minute), now.Add(time.Minute)
			cands = append(cands, c)
		}
		o := Options{Now: now, Hover: hover}
		if !hover {
			o.Threshold = 80
		}
		p := Decide(cands, o, Config{}, st, live)
		if p.Action == ActionSwitch {
			live, switches = p.Target.UUID, switches+1
			st.RecordSwitch(live, now)
		}

		for j := range accts {
			if accts[j].uuid != live {
				continue
			}
			// The turn is served only if BOTH windows can take it. A five-hour
			// window at its limit stops the session however much of the week is
			// left, which is the constraint that makes the staggering matter.
			w := weeklyPerTick
			if room := 100 - accts[j].weekUtil; w > room {
				w = room
			}
			if room := (100 - accts[j].fiveUtil) / fiveHoursPerWeek; w > room {
				w = room
			}
			if w <= 0 {
				break
			}
			accts[j].weekUtil += w
			accts[j].fiveUtil += w * fiveHoursPerWeek
			accts[j].served += w
			break
		}
	}

	total := 0.0
	for _, a := range accts {
		total += a.wasted
		t.Logf("   %-10s served %6.2f  wasted at rollover %6.2f", a.uuid, a.served, a.wasted)
	}
	t.Logf("%s: %d switches, %.2f weekly points thrown away", label, switches, total)
	return accts, total
}

// staggeredFleet is the shape of the pool that reported the defect: four
// accounts barely into their weeks, with those weeks ending 15, 22, 48 and 90
// hours out, and five-hour windows on two reset cohorts ten minutes apart.
func staggeredFleet() []wasteAccount {
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	return []wasteAccount{
		{uuid: "a-90h", weekUtil: 2, fiveUtil: 8, weekAt: base.Add(90 * time.Hour), fiveAt: base.Add(4*time.Hour + 7*time.Minute)},
		{uuid: "b-48h", weekUtil: 2, fiveUtil: 7, weekAt: base.Add(48 * time.Hour), fiveAt: base.Add(4*time.Hour + 7*time.Minute)},
		{uuid: "c-22h", weekUtil: 1, fiveUtil: 4, weekAt: base.Add(22 * time.Hour), fiveAt: base.Add(4*time.Hour + 17*time.Minute)},
		{uuid: "d-15h", weekUtil: 1, fiveUtil: 5, weekAt: base.Add(15 * time.Hour), fiveAt: base.Add(4*time.Hour + 17*time.Minute)},
	}
}

// The fix, stated as the work it moves onto the deadline.
//
// The run stops at the FIRST weekly rollover, and that bound is the whole
// measurement rather than a convenience. What the change claims is that quota
// about to expire gets spent while it still exists, so the quantity to measure
// is what each account absorbed BEFORE the deadline. Run past it and the figure
// reverses on its own and says nothing: an account that has just been given
// extra turns sits that much further up its five-hour window afterwards, ranks
// last for a while, and gives the work back. That is correct and it is not what
// this case is asking about.
//
// What the change does NOT claim, and what an earlier version of this case
// wrongly asserted, is that the total thrown away falls to near zero. It cannot.
// The five-hour window is 33.6 times shorter than the week, so one account can
// absorb at most about 3 weekly points per five-hour window whatever the
// ranking says -- roughly 9 points in the fifteen hours this account has left,
// against the 99 it is holding. Ninety of those points were never reachable by
// any engine, and a test that credited the ordering with saving them would be
// measuring the fixture's demand rather than the fix.
//
// Measured here: the two accounts whose weeks expire inside the run absorb 6.55
// and 6.50 points against 4.65 and 4.80 for the two with days left, a 38% shift
// onto the deadline.
func TestHoverSpendsAWeekThatWouldOtherwiseExpireUnspent(t *testing.T) {
	// 450 ticks of two minutes is fifteen hours, which is exactly when the
	// first week ends.
	final, _ := runWasteSim(t, "hover, a staggered four-account fleet up to the first weekly rollover",
		staggeredFleet(), true, 0.05, 450)

	by := map[string]wasteAccount{}
	for _, a := range final {
		by[a.uuid] = a
	}
	for _, soon := range []string{"d-15h", "c-22h"} {
		for _, later := range []string{"a-90h", "b-48h"} {
			if by[soon].served <= by[later].served {
				t.Errorf("%s absorbed %.2f and %s absorbed %.2f: the engine is spreading the work "+
					"evenly over a deadline it cannot see",
					soon, by[soon].served, later, by[later].served)
			}
		}
	}
	// The size of the shift, not only its direction. A tie-break that moved one
	// tick would satisfy the comparisons above and save nothing.
	deadline := by["d-15h"].served + by["c-22h"].served
	spare := by["a-90h"].served + by["b-48h"].served
	if deadline < spare*1.25 {
		t.Errorf("the two accounts with deadlines absorbed %.2f against %.2f for the two without, "+
			"want at least a quarter more", deadline, spare)
	}
}
