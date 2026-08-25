package forecast

import (
	"fmt"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// runBounded runs one simulation and fails the test if it does not return.
//
// The bound is what makes the wrong implementation observable, and it is why
// every call in this file goes through here. A simulation that picks an account
// with no room computes a zero-length interval, advances no clock and never
// returns a wrong answer -- it returns nothing at all. A plain call would hang
// the package's whole test binary until the go test deadline and name nothing;
// this names the fault and lets the rest of the file run.
//
// Five seconds is four orders of magnitude above what these fixtures need. The
// longest run here is a six-account fleet whose five-hour windows roll over
// every five hours across fourteen days, which is some four hundred iterations
// of floating-point arithmetic and finishes in well under a millisecond.
func runBounded(t *testing.T, run func() (time.Time, bool, map[string]time.Time)) (time.Time, bool, map[string]time.Time) {
	t.Helper()
	type result struct {
		dryAt   time.Time
		dry     bool
		emptyAt map[string]time.Time
	}
	done := make(chan result, 1)
	go func() {
		dryAt, dry, emptyAt := run()
		done <- result{dryAt, dry, emptyAt}
	}()
	select {
	case r := <-done:
		return r.dryAt, r.dry, r.emptyAt
	case <-time.After(5 * time.Second):
		t.Fatal("the simulation did not return within five seconds; a step that advances no clock spins here rather than answering")
		return time.Time{}, false, nil
	}
}

// Six accounts every one of which is out is the ORDINARY end state of this
// rotation -- internal/strategy/drain_test.go asserts that the engine spends the
// pool over a week rather than stopping with quota left in it. An account at
// 100% is perfectly readable, so a usability test that asks only about
// readability picks one, finds its earliest window already at the limit,
// computes a zero-length interval, advances no clock, and every surface that
// prints a runway spins forever.
//
// The recovery half is the other decision: a fleet that is out now and rolls
// over inside the horizon has not run dry. Reporting it as dry would answer a
// question about this minute rather than about whether the rate is sustainable.
func TestASpentFleetTerminatesAndReportsItsRecovery(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	accts := make([]simAccount, 0, 6)
	for i := range 6 {
		accts = append(accts, simAccount{
			uuid: fmt.Sprintf("uuid-%d", i),
			idx:  i + 1,
			windows: map[usage.WindowName]simWindow{
				usage.WindowFiveHour: {pct: 100, reset: now.Add(time.Duration(i+1) * 40 * time.Minute), length: 5 * time.Hour},
			},
		})
	}
	rates := map[usage.WindowName]float64{usage.WindowFiveHour: 10}
	_, dry, emptyAt := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	if dry {
		t.Fatal("a fleet whose five-hour windows all roll over inside forty minutes was reported dry")
	}
	// Out at the start is still out, and the caller has to be able to say so.
	// Every one of the six, not just the first: the first is also the first to
	// roll over, so an implementation that overwrote each account's moment
	// with the latest one it saw would still leave that one reading "now".
	for i := range 6 {
		uuid := fmt.Sprintf("uuid-%d", i)
		if got, ok := emptyAt[uuid]; !ok || !got.Equal(now) {
			t.Errorf("emptyAt[%s] = %v, %v; an account already spent is empty now, not at the later moment the run last looked at it", uuid, got, ok)
		}
	}
}

// Burn goes to one account at a time, because ccdad has one live login at a
// time. Spreading the fleet rate across every account in parallel reports a dry
// moment roughly n times too early, and on a six-account fleet that is the
// difference between "one hour" and "this evening".
func TestTheFleetBurnsOneAccountAtATime(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	// Six accounts, weekly windows at 0%, resets outside the horizon so nothing
	// replenishes. At 100 points per hour the fleet holds 600 points = 6 hours.
	accts := make([]simAccount, 0, 6)
	for i := range 6 {
		accts = append(accts, simAccount{
			uuid: fmt.Sprintf("uuid-%d", i), idx: i + 1,
			windows: map[usage.WindowName]simWindow{
				usage.WindowSevenDay: {pct: 0, reset: now.Add(20 * 24 * time.Hour), length: 7 * 24 * time.Hour},
			},
		})
	}
	rates := map[usage.WindowName]float64{usage.WindowSevenDay: 100}
	dryAt, dry, _ := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	if !dry {
		t.Fatal("dry = false")
	}
	if got, want := dryAt.Sub(now), 6*time.Hour; got != want {
		t.Fatalf("dry after %v, want %v -- burning every account in parallel would give %v", got, want, want/6)
	}
}

// The run names the moment each account ran out, because a fleet answer alone
// cannot tell a user which login to stop reaching for.
func TestTheRunNamesTheMomentEachAccountRanOut(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	accts := make([]simAccount, 0, 3)
	for i := range 3 {
		accts = append(accts, simAccount{
			uuid: fmt.Sprintf("uuid-%d", i), idx: i + 1,
			windows: map[usage.WindowName]simWindow{
				usage.WindowSevenDay: {pct: 0, reset: now.Add(20 * 24 * time.Hour), length: 7 * 24 * time.Hour},
			},
		})
	}
	rates := map[usage.WindowName]float64{usage.WindowSevenDay: 100}
	_, _, emptyAt := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	// One account at a time, in Idx order, an hour each.
	for i := range 3 {
		uuid := fmt.Sprintf("uuid-%d", i)
		want := now.Add(time.Duration(i+1) * time.Hour)
		got, ok := emptyAt[uuid]
		if !ok || !got.Equal(want) {
			t.Errorf("emptyAt[%s] = %v, %v; want %v", uuid, got, ok, want)
		}
	}
}

// A window with a readable percentage and no reported reset is burned like any
// other. strategy.HeadroomFor never consults Reset(), and neverSpent's own doc
// in internal/strategy/headroom.go says why: a window above zero with no reset
// is a resets_at this build could not read, not an unused window. Freezing it
// would be fail-open on the one axis this package chose to fail closed on -- it
// would hand the fleet an account that cannot be spent and cannot recover.
func TestAWindowWithNoResetIsBurnedNotFrozen(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	accts := []simAccount{{
		uuid: "uuid-a", idx: 1,
		windows: map[usage.WindowName]simWindow{
			usage.WindowSevenDay: {pct: 97, length: 7 * 24 * time.Hour}, // zero reset
		},
	}}
	rates := map[usage.WindowName]float64{usage.WindowSevenDay: 3}
	dryAt, dry, _ := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	if !dry {
		t.Fatal("an account three points from spent, with nothing to roll it over, was reported to hold")
	}
	if got, want := dryAt.Sub(now), time.Hour; got != want {
		t.Fatalf("dry after %v, want %v", got, want)
	}
}

// A rate of zero is a rate. A window at zero rate never reaches 100 and must
// contribute NO event rather than an infinite one: time.Duration(math.Inf(1))
// is a large NEGATIVE count on the platforms this ships to, so an infinite
// event does not park the clock at the horizon, it runs the clock backwards.
func TestAZeroRateContributesNoEvent(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	accts := []simAccount{{
		uuid: "uuid-a", idx: 1,
		windows: map[usage.WindowName]simWindow{
			usage.WindowSevenDay: {pct: 50, reset: now.Add(20 * 24 * time.Hour), length: 7 * 24 * time.Hour},
		},
	}}
	rates := map[usage.WindowName]float64{usage.WindowSevenDay: 0}
	_, dry, _ := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	if dry {
		t.Fatal("a fleet burning nothing was reported to run dry")
	}
}

// Scope decides what burns; usability is always fleet-wide. A run scoped to the
// weekly axis must not hand work to an account whose FIVE-HOUR window is at 100
// -- scoping usability along with the burn makes every per-axis verdict
// optimistic in exactly the way a fail-closed forecast refuses to be, because it
// counts an account that cannot be switched to.
//
// This is also the mechanism that makes a verdict differ from the replenishment
// comparison, and it is why the verdict may not be wired to that comparison.
// Here the comparison over six accounts is 6 x 100/168h = 3.57 points per hour
// against a burn of 3, which "holds" -- and the axis still runs dry, because
// four of the six supply nothing. The two that do supply 1.19.
func TestAnAccountOutOnTheOtherAxisSuppliesNothingToThisOne(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	accts := make([]simAccount, 0, 6)
	for i := range 6 {
		fiveHour := 0.0
		if i >= 2 {
			// Out on the five-hour axis, and staying out: that window is not in
			// this run's scope, so nothing in the run rolls it back.
			fiveHour = 100
		}
		accts = append(accts, simAccount{
			uuid: fmt.Sprintf("uuid-%d", i), idx: i + 1,
			windows: map[usage.WindowName]simWindow{
				usage.WindowFiveHour: {pct: fiveHour, reset: now.Add(4 * time.Hour), length: 5 * time.Hour},
				usage.WindowSevenDay: {pct: 0, reset: now.Add(24 * time.Hour), length: 7 * 24 * time.Hour},
			},
		})
	}
	rates := map[usage.WindowName]float64{usage.WindowSevenDay: 3}
	scope := []usage.WindowName{usage.WindowSevenDay}
	_, dry, _ := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulateScoped(accts, rates, scope, now)
	})
	if !dry {
		t.Fatal("two usable accounts at 3 points per hour were reported to hold; the four that are out on the five-hour axis supplied weekly capacity nothing can reach")
	}
}

// An unreadable account is not usable. Fail closed: a runway that counts an
// account nobody can see is a promise the fleet may not be able to keep.
func TestAnUnreadableAccountIsNotUsable(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	accts := []simAccount{
		{uuid: "readable", idx: 1, windows: map[usage.WindowName]simWindow{
			usage.WindowSevenDay: {pct: 0, reset: now.Add(20 * 24 * time.Hour), length: 7 * 24 * time.Hour}}},
		{uuid: "unreadable", idx: 2, windows: nil}, // no readable window at all
	}
	rates := map[usage.WindowName]float64{usage.WindowSevenDay: 100}
	dryAt, dry, emptyAt := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	if !dry {
		t.Fatal("dry = false")
	}
	if got, want := dryAt.Sub(now), time.Hour; got != want {
		t.Fatalf("dry after %v, want %v -- the unreadable account must supply no runway", got, want)
	}
	// Unreadable is not empty. An account nobody can see has no moment to name,
	// and inventing one would report a fact about a fleet that was never read.
	if at, ok := emptyAt["unreadable"]; ok {
		t.Errorf("emptyAt[unreadable] = %v; an account with no readable window ran out of nothing", at)
	}
}

// A reset that has already passed still rolls the window over.
//
// The snapshot the run starts from can be older than the event: ColdWindow in
// internal/strategy/headroom.go carries an arm for exactly this reading, "the
// window ran down; the cached reading is simply older than the event". A run
// that only fires rollovers strictly in its own future would leave such a window
// with no rollover at all and silently turn a periodic quota into a one-shot
// balance -- the five-hour axis would be reported as though it never refilled.
//
// The utilization is NOT reset by that repair. The level is what was read; only
// the schedule is inferred, and granting quota nobody observed is the fail-open
// half of this.
func TestAResetAlreadyPastStillRollsTheWindowOver(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	accts := []simAccount{{
		uuid: "uuid-a", idx: 1,
		windows: map[usage.WindowName]simWindow{
			// Read 90% four hours ago, with a reset an hour older than that: the
			// window's real boundaries are at now+1h, now+6h, and so on.
			usage.WindowFiveHour: {pct: 90, reset: now.Add(-4 * time.Hour), length: 5 * time.Hour},
		},
	}}
	rates := map[usage.WindowName]float64{usage.WindowFiveHour: 40}
	_, dry, emptyAt := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	// 10 points of room at 40 points per hour is fifteen minutes, so the account
	// empties before its boundary and the run must still see it recover.
	if got, ok := emptyAt["uuid-a"]; !ok || !got.Equal(now.Add(15*time.Minute)) {
		t.Errorf("emptyAt[uuid-a] = %v, %v; want %v", got, ok, now.Add(15*time.Minute))
	}
	if dry {
		t.Fatal("a five-hour window whose reset was stale was reported never to roll over again")
	}
}

// The run may not spend the caller's fleet. Six runs share one input -- three
// scopes, each at both ends of the measured band -- and a run that burned its
// argument would hand the next one a fleet the previous one had already emptied,
// so the second reading of the same fleet would always be the bleaker one.
func TestTheRunDoesNotSpendTheCallersFleet(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	accts := []simAccount{{
		uuid: "uuid-a", idx: 1,
		windows: map[usage.WindowName]simWindow{
			usage.WindowSevenDay: {pct: 10, reset: now.Add(2 * time.Hour), length: 7 * 24 * time.Hour},
		},
	}}
	rates := map[usage.WindowName]float64{usage.WindowSevenDay: 30}
	first, _, _ := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	if got := accts[0].windows[usage.WindowSevenDay]; got.pct != 10 || !got.reset.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("the input was mutated: pct = %v, reset = %v", got.pct, got.reset)
	}
	second, _, _ := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	if !first.Equal(second) {
		t.Fatalf("the same fleet answered %v and then %v", first, second)
	}
}

// A rollover that puts nobody back in service is not a recovery, and the moment
// the fleet stopped being able to take work is the moment it ran dry.
//
// This is the difference between "the fleet is out and cannot recover" and "the
// fleet is out and something somewhere rolls over every five hours". An account
// whose weekly cap is blown is out no matter how many times its five-hour window
// refills, so a run that stopped at the first rollover of any kind would walk
// its clock all the way to the far end of the horizon and then report the drying
// moment as thirteen days away -- for a fleet that is dry right now.
func TestARolloverThatRestoresNobodyIsNotARecovery(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	accts := []simAccount{{
		uuid: "uuid-a", idx: 1,
		windows: map[usage.WindowName]simWindow{
			// Healthy and rolling over every five hours, which is what makes
			// this fixture the trap: there is always a rollover ahead.
			usage.WindowFiveHour: {pct: 0, reset: now.Add(time.Hour), length: 5 * time.Hour},
			// Spent, with nothing inside the horizon to roll it back.
			usage.WindowSevenDay: {pct: 100, reset: now.Add(20 * 24 * time.Hour), length: 7 * 24 * time.Hour},
		},
	}}
	rates := map[usage.WindowName]float64{usage.WindowFiveHour: 10, usage.WindowSevenDay: 1}
	dryAt, dry, _ := runBounded(t, func() (time.Time, bool, map[string]time.Time) {
		return simulate(accts, rates, now)
	})
	if !dry {
		t.Fatal("an account whose weekly cap is blown until next month was reported to hold, because its five-hour window keeps refilling")
	}
	if !dryAt.Equal(now) {
		t.Fatalf("dry at %v, want %v -- the fleet stopped being able to take work now, not at the last five-hour rollover before the horizon", dryAt.Sub(now), time.Duration(0))
	}
}
