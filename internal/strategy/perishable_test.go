package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The fleet of 2026-09-05, which is where the defect was reported and measured.
//
// Four accounts, every one of them barely into its five-hour window and barely
// into its week. What separates them is WHEN the week ends: 15, 22, 48 and 90
// hours out. The account whose week expires first holds 99 of its 100 weekly
// points and has fifteen hours to spend them in; nothing in the pool can absorb
// them but that account, and after the reset they are gone.
//
// Ranked on the binding window alone the pool spans 1.327 points of five-hour
// pace and orders 48h, 22h, 90h, 15h -- uncorrelated with the expiry, and with
// the account about to lose a whole week LAST. That is the reported bug, and
// the numbers below are the ones `cmd/hoverdiag` read off the live engine at
// 2026-09-05T05:52:45+09:00 rather than a shape invented for the test.
const (
	fiveHourLen = 5 * time.Hour
	weekLen     = 7 * 24 * time.Hour
)

// perishableFleet is that pool. The two cohorts of five-hour reset instant are
// real -- two accounts were polled ten minutes after the other two, so they
// share an elapsed share and their thresholds are identical, which is what left
// a single point of utilization deciding the order.
func perishableFleet() []Candidate {
	acct := func(uuid string, fiveElapsed, fivePct, weekElapsed, weekPct float64) Candidate {
		return sub(uuid, &usage.Snapshot{
			FiveHour: elapsedWindow(fiveHourLen, fiveElapsed, fivePct),
			SevenDay: elapsedWindow(weekLen, weekElapsed, weekPct),
		})
	}
	return []Candidate{
		acct("1-kweizaa", 0.14256, 4, 0.86833, 1),     // week ends in 22h07m
		acct("2-tlfyvhsdlek", 0.17583, 7, 0.71357, 2), // week ends in 48h07m
		acct("3-official", 0.17589, 8, 0.46357, 2),    // week ends in 90h07m
		acct("4-chan", 0.14256, 5, 0.91000, 1),        // week ends in 15h07m
	}
}

// The reported bug, stated as the ordering it produced.
//
// The premise is asserted first and it is not ceremony: the whole defect is that
// the perishable window never binds, so a fixture in which seven_day DID bind
// would pass this test while testing nothing. Every account here must bind on
// five_hour, and then the account whose week expires first must still lead.
func TestHoverRaisesTheShareOfAnAccountItsRotationCannotDrain(t *testing.T) {
	res := Rank(perishableFleet(), hoverOpts())

	for _, r := range res.Order {
		if r.Headroom.Binding != usage.WindowFiveHour {
			t.Fatalf("%s binds on %s, want five_hour: the premise of this test is that the perishable window does NOT bind", r.UUID, r.Headroom.Binding)
		}
	}
	eq(t, order(res), []string{"4-chan", "1-kweizaa", "2-tlfyvhsdlek", "3-official"})
}

// The same pool through Decide, which is the assertion a comparator-only fix
// leaves red: the ranking would put the perishable account first and the
// additive margin, measured on the same slack, would then refuse to move onto
// it. Ordering is not switching, and the bug the user reported is that the
// engine did not switch.
func TestHoverSwitchesToTheAccountWhoseWeekIsAboutToBeStranded(t *testing.T) {
	p := Decide(perishableFleet(), hoverOpts(), Config{}, NewState(), "1-kweizaa")

	if p.Action != ActionSwitch {
		t.Fatalf("Action = %v (%s), want a switch onto the account whose week expires first", p.Action, p.Reason)
	}
	if p.Target.UUID != "4-chan" {
		t.Errorf("Target = %s, want 4-chan", p.Target.UUID)
	}
}

// The share widens for exactly the accounts that cannot be drained in time, and
// by exactly the quota that would otherwise expire. Two of these four strand
// nothing and keep the flat pool slice, which is what makes the term a targeted
// licence rather than a general loosening.
func TestTheWidenedShareIsTheQuotaTheRotationCannotReach(t *testing.T) {
	p := HoverThresholds(perishableFleet(), hoverOpts())

	for _, tc := range []struct {
		uuid string
		want float64
	}{
		// 99 points left, 9% of the week to spend them in, four accounts
		// sharing the rotation: 99 - 4x9 = 63 points nobody can reach.
		{"4-chan", 63},
		// 99 left and 13.167% of the week: 99 - 4x13.167 = 46.332.
		{"1-kweizaa", 46.332},
		// 98 left and 28.643% of the week to spend them in -- the rotation
		// absorbs 114.6, so nothing strands and the flat share stands.
		{"2-tlfyvhsdlek", 25},
		{"3-official", 25},
	} {
		got := p.ShareFor(tc.uuid)
		if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("ShareFor(%s) = %v, want %v", tc.uuid, got, tc.want)
		}
	}
}

// The identity the mode's auditability rests on: every derived threshold is
// still this window's elapsed share plus this ACCOUNT's share, with both terms
// on the table. What changed is that the second term is no longer one number
// for the whole pool, which is why HoverAccount had to exist.
func TestTheDerivedThresholdIsStillTheElapsedShareAndTheShare(t *testing.T) {
	p := HoverThresholds(perishableFleet(), hoverOpts())

	for _, row := range p.Windows {
		if !row.HasExpected {
			continue
		}
		want := row.ExpectedPct + p.ShareFor(row.UUID)
		if diff := row.Threshold - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s %s: threshold %v, want ExpectedPct %v + share %v = %v",
				row.UUID, row.Window, row.Threshold, row.ExpectedPct, p.ShareFor(row.UUID), want)
		}
	}
}

// A pool that is keeping up with its weeks strands nothing, so the term is
// inert and every account gets the flat slice it got before this existed.
//
// The three fixtures are the pools the mode was tuned on. If the term fired on
// any of them it would be a general loosening wearing a targeted name, and the
// drain tests that pin hover's pace behaviour would be measuring something else.
func TestAPoolDrainingAtPaceGetsTheFlatShare(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool []Candidate
	}{
		{"the live pool of 2026-08-24", livePoolOf20260824()},
		{"the fleet of 2026-08-25", fleetOf20260825()},
		{"the burn pool", hoverBurnPool()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := HoverThresholds(tc.pool, hoverOpts())
			flat := 100 / float64(p.Usable)
			for _, a := range p.Accounts {
				if a.Stranded != 0 {
					t.Errorf("%s stranded %v, want 0: this pool is spending its weeks", a.UUID, a.Stranded)
				}
				if a.Share != flat {
					t.Errorf("%s share = %v, want the flat %v", a.UUID, a.Share, flat)
				}
			}
		})
	}
}

// The licence may never exceed the quota the account actually holds, and this
// sweeps for it rather than asserting one case. A share above 100 would licence
// spending an account does not have; a share below the flat slice would make the
// term a penalty; and a widening larger than the room left would price quota the
// account cannot serve.
func TestTheStrandedShareNeverExceedsTheQuotaTheAccountHolds(t *testing.T) {
	for _, usable := range []int{1, 2, 4, 10, 50} {
		for elapsed := 0.0; elapsed <= 100; elapsed += 5 {
			for util := 0.0; util <= 100; util += 5 {
				s := &usage.Snapshot{SevenDay: elapsedWindow(weekLen, elapsed/100, util)}
				_, stranded, _ := hoverStranded(s, "", opts().Thresholds(), usable, now, 0, false)
				share := hoverShare(usable, stranded)
				flat := hoverPoolShare(usable)
				switch {
				case share > 100:
					t.Fatalf("usable %d elapsed %v util %v: share %v exceeds a whole window", usable, elapsed, util, share)
				case share < flat:
					t.Fatalf("usable %d elapsed %v util %v: share %v is under the flat %v", usable, elapsed, util, share, flat)
				case share-flat > 100-util+1e-9:
					t.Fatalf("usable %d elapsed %v util %v: widened by %v, more than the %v the account holds", usable, elapsed, util, share-flat, 100-util)
				}
			}
		}
	}
}

// An account with nothing left in the week strands nothing, because there is
// nothing to strand. This is the boundary the room cap and the empty tier have
// to agree on: a spent account must not be handed a wider licence for holding
// quota it does not hold.
func TestAnEmptyWeeklyStrandsNothing(t *testing.T) {
	s := &usage.Snapshot{SevenDay: elapsedWindow(weekLen, 0.91, 100)}
	if _, stranded, _ := hoverStranded(s, "", opts().Thresholds(), 4, now, 0, false); stranded != 0 {
		t.Errorf("stranded = %v, want 0 for a week with nothing left in it", stranded)
	}
}

// A weekly whose reset is already behind us is a window that HAS rolled over and
// whose refresh has not landed. Its surplus is gone rather than perishing, and
// pricing it as urgent would hand the largest licence the system can express to
// an account at the exact instant its urgency ended -- on a figure the next poll
// deletes.
func TestAWeeklyThatHasAlreadyRolledStrandsNothing(t *testing.T) {
	stale := usage.NewWindow(ptr(1.0), ptr(now.Add(-2*time.Hour)))
	s := &usage.Snapshot{SevenDay: stale}

	_, stranded, has := hoverStranded(s, "", opts().Thresholds(), 4, now, 0, false)
	if has || stranded != 0 {
		t.Errorf("stranded = %v (has %v), want 0: the window has already rolled", stranded, has)
	}
}

// The floor and the empty tier read the same window set and agree in the
// direction that matters: an account OutOfQuota files empty is one the floor
// has already refused to widen. They no longer go to zero at the same instant
// -- the floor fires first, while room is still positive -- and the two sliver
// rows are exactly the band the old equality between the room figure and
// OutOfQuota (fe1a5fe) could not express. The blown-sub-cap row is why both
// range over the all-model windows first: a spent Fable cap beside a week with
// room may not zero a licence OutOfQuota would not zero. The half-a-point row
// is what stops the floor being a constant: 0.5 of a week is fifty minutes of
// work, and reading it against the five-hour figure would throw it away.
func TestTheLicenceFloorFiresBeforeTheEmptyTierDoes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		s       *usage.Snapshot
		absorbs bool
	}{
		{"room in both", snap(win(10, time.Hour), win(20, weekLen/2)), true},
		{"the week is empty", snap(win(10, time.Hour), win(100, weekLen/2)), false},
		{"the five-hour window is empty", snap(win(100, time.Hour), win(20, weekLen/2)), false},
		{"a blown sub-cap beside a week with room", &usage.Snapshot{
			FiveHour:     win(10, time.Hour),
			SevenDay:     win(20, weekLen/2),
			SevenDayOpus: win(100, weekLen/2),
		}, true},
		{"a sliver of a five-hour window", snap(win(99.9, time.Hour), win(20, weekLen/2)), false},
		{"a sliver of a week", snap(win(10, time.Hour), win(99.99, weekLen/2)), false},
		{"half a point of a week", snap(win(10, time.Hour), win(99.5, weekLen/2)), true},
		{"nothing but a spent model cap", &usage.Snapshot{SevenDayOpus: win(100, weekLen/2)}, false},
		{"nothing but a model cap with room", &usage.Snapshot{SevenDayOpus: win(20, weekLen/2)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := opts().Thresholds()
			got := absorbsACooldown(tc.s, "", th, 0, false)
			if got != tc.absorbs {
				t.Errorf("absorbsACooldown = %v, want %v", got, tc.absorbs)
			}
			empty, known := OutOfQuota(HeadroomFor(tc.s, "", th))
			if !known {
				t.Fatal("OutOfQuota could not answer for a readable snapshot")
			}
			if empty && got {
				t.Error("OutOfQuota files the account empty but the floor would still widen its licence")
			}
		})
	}
}

// ptr is a pointer to a value, for the windows that state a reset instant
// directly rather than as a share of a length.
func ptr[T any](v T) *T { return &v }

// What is left of the room cap, and where it stops.
//
// The fixture is the one the cap was added for in fe1a5fe: ten points of
// five-hour room beside a week 91% elapsed at 1% used, 99 points that nothing
// but this account can reach. The cap read ten points as "cannot serve the next
// session" and refused the licence. ccdad does not hand out sessions; it moves
// after HoverCooldown, and ten points is thirty minutes of work with a fresh
// window half an hour out -- fifteen cooldowns -- so the licence is 81 and
// Decide switches onto it. Driven forward to the weekly rollover that is 51
// switches against 63 and 9.23 weekly points absorbed against 8.93, on a
// fixture drain_test.go already puts the physical ceiling of at about 9.
//
// What a widening may not do is manufacture a lead for an account that cannot
// pay for the switch: the 0.7 and 0.6 rows straddle one cooldown of a five-hour
// window, 0.667 points, and below it the licence is refused, the share falls
// back to the flat slice and the engine stays. At zero it is the empty tier,
// not the floor, that files the account last.
func TestAnAccountThatCannotAbsorbOneCooldownGetsNoWidening(t *testing.T) {
	for _, tc := range []struct {
		name     string
		room     float64
		stranded float64
		order    []string
		action   Action
	}{
		{"ten points, fifteen cooldowns", 10, 81, []string{"a-nearly-empty", "b-roomy"}, ActionSwitch},
		{"just over one cooldown", 0.7, 81, []string{"a-nearly-empty", "b-roomy"}, ActionSwitch},
		{"just under one cooldown", 0.6, 0, []string{"b-roomy", "a-nearly-empty"}, ActionStay},
		{"nine seconds of work", 0.05, 0, []string{"b-roomy", "a-nearly-empty"}, ActionStay},
		{"nothing at all", 0, 0, []string{"b-roomy", "a-nearly-empty"}, ActionStay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nearly := sub("a-nearly-empty", &usage.Snapshot{
				FiveHour: elapsedWindow(fiveHourLen, 0.90, 100-tc.room),
				SevenDay: elapsedWindow(weekLen, 0.91, 1),
			})
			// Sixty points of week left and half the week to spend them in,
			// so its rotation reaches all of it and nothing strands.
			roomy := sub("b-roomy", &usage.Snapshot{
				FiveHour: elapsedWindow(fiveHourLen, 0.70, 10),
				SevenDay: elapsedWindow(weekLen, 0.50, 40),
			})
			pool := []Candidate{nearly, roomy}

			p := HoverThresholds(pool, hoverOpts())
			a, ok := p.AccountFor("a-nearly-empty")
			if !ok {
				t.Fatal("no derivation for a-nearly-empty")
			}
			if diff := a.Stranded - tc.stranded; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("stranded = %v, want %v", a.Stranded, tc.stranded)
			}
			if tc.stranded == 0 && a.Share != a.PoolShare {
				t.Errorf("share = %v against a pool slice of %v: a refused licence must fall back to the flat slice", a.Share, a.PoolShare)
			}
			eq(t, order(Rank(pool, hoverOpts())), tc.order)
			if d := Decide(pool, hoverOpts(), Config{}, NewState(), "b-roomy"); d.Action != tc.action {
				t.Errorf("Action = %v (%s), want %v", d.Action, d.Reason, tc.action)
			}
		})
	}
}

// The fleet of 2026-09-05T14:40+09:00, as the live cache reported it: six
// usable accounts, a pool slice of 16.667, and one five-hour cohort 90.56%
// elapsed with 28 minutes to run. mintc.chan holds 61 points of a week that
// expires in 6h18m -- 38.5 of them beyond what six accounts can absorb in the
// 3.75% of the week left -- and its five-hour window is 91% used. The cap
// clamped its licence to the nine five-hour points, under the pool slice, and
// it ranked LAST of six.
//
// It does not reach first, and that is the binding window doing its job: 91%
// used is 91% used for the next half hour. What it may not be is sixth, behind
// four accounts with days of week left. Rolling the five-hour windows -- the
// same fleet 28 minutes later, no work done -- puts it ahead of every account
// that has a week at all, with the same stranded figure. That is the clock
// proof: the clamp measured which window happened to be near its reset, not
// the account.
//
// The one account it still trails after the roll is ejalrnrmf, added minutes
// earlier and never spent against, so no window of its reports a reset and
// every one takes the assumed elapsed share. A fresh account leading on a
// guess is a different question -- HoverUnknownElapsedPct -- and it is not
// asserted here either way.
func fleetOf20260905T1440(fiveElapsed float64) []Candidate {
	five := func(pct float64) usage.Window {
		if fiveElapsed < 0.01 {
			pct = 0 // a window that has just rolled holds nothing
		}
		return elapsedWindow(fiveHourLen, fiveElapsed, pct)
	}
	week := func(hoursLeft, pct float64) usage.Window {
		return elapsedWindow(weekLen, 1-hoursLeft/168, pct)
	}
	zero := 0.0
	return []Candidate{
		sub("ejalrnrmf", &usage.Snapshot{
			FiveHour: usage.NewWindow(&zero, nil),
			SevenDay: usage.NewWindow(&zero, nil),
		}),
		sub("tlfyvhsdlek", &usage.Snapshot{FiveHour: five(60), SevenDay: week(39.3, 32)}),
		sub("kweizaa", &usage.Snapshot{FiveHour: five(69), SevenDay: week(13.3, 35.02)}),
		sub("mintc.official", &usage.Snapshot{FiveHour: five(48), SevenDay: week(81.3, 30)}),
		sub("mintc.junseong", &usage.Snapshot{
			FiveHour: elapsedWindow(fiveHourLen, 0.0361, 0),
			SevenDay: week(145.3, 0),
		}),
		sub("mintc.chan", &usage.Snapshot{FiveHour: five(91), SevenDay: week(6.3, 39)}),
	}
}

func TestAPerishableWeekOutlivesAFiveHourWindowAboutToRoll(t *testing.T) {
	// As reported: the cohort 90.56% through its five-hour window.
	pool := fleetOf20260905T1440(0.9056)
	p := HoverThresholds(pool, hoverOpts())
	chan1, _ := p.AccountFor("mintc.chan")
	if diff := chan1.Stranded - 38.5; diff > 0.05 || diff < -0.05 {
		t.Errorf("stranded = %v, want the 38.5 points of the week that expires in six hours", chan1.Stranded)
	}
	if chan1.Share <= chan1.PoolShare {
		t.Errorf("share = %v against a pool slice of %v: the widening vanished", chan1.Share, chan1.PoolShare)
	}
	got := order(Rank(pool, hoverOpts()))
	if got[len(got)-1] == "mintc.chan" {
		t.Fatalf("order = %v: the account whose week expires first is last", got)
	}

	// The same fleet 28 minutes later, every five-hour window just rolled, no
	// work done. The stranded figure must not have moved, because nothing
	// about the account did.
	rolled := fleetOf20260905T1440(0.001)
	p2 := HoverThresholds(rolled, hoverOpts())
	chan2, _ := p2.AccountFor("mintc.chan")
	if diff := chan2.Stranded - chan1.Stranded; diff > 0.05 || diff < -0.05 {
		t.Errorf("stranded moved from %v to %v with no work done: the licence was measuring a clock", chan1.Stranded, chan2.Stranded)
	}
	got2 := order(Rank(rolled, hoverOpts()))
	pos := -1
	for i, u := range got2 {
		if u == "mintc.chan" {
			pos = i
		}
	}
	if pos != 1 || got2[0] != "ejalrnrmf" {
		t.Errorf("order after the roll = %v: want mintc.chan ahead of every account that has a week, behind only the never-spent one", got2)
	}
}

// The perishable window is the one a MODEL CHOICE CANNOT DODGE, and the soonest
// reset decides only among those.
//
// An untouched cap on one model family, beside an all-model week that is nearly
// spent, would otherwise buy the front of the queue for quota the session was
// never going to run: the sub-cap resets sooner and holds everything, so plain
// soonest-reset picks it and prices a hundred points as perishable.
func TestTheStrandedWindowIsTheOneAModelChoiceCannotDodge(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: elapsedWindow(fiveHourLen, 0.30, 10),
		// The all-model week: 90% spent, half elapsed, and it resets LAST.
		SevenDay: elapsedWindow(weekLen, 0.50, 90),
		// One model family's own cap: untouched, nearly elapsed, resets FIRST.
		SevenDayOpus: elapsedWindow(weekLen, 0.91, 0),
	}

	w, stranded, has := hoverStranded(s, "", opts().Thresholds(), 2, now, 0, false)
	if !has || w.Name != usage.WindowSevenDay {
		t.Fatalf("priced %s, want seven_day: a cap on one model family cannot be the perishable window while an all-model week is readable", w.Name)
	}
	if stranded != 0 {
		t.Errorf("stranded = %v, want 0: the week the session actually spends is 90 percent gone", stranded)
	}

	// The same rule with the wire order REVERSED, which is the half that
	// actually tests the preference. The fixed windows are walked in the
	// schema's order, so an all-model week is normally reached before any
	// per-model cap and a rule that merely took the first readable weekly would
	// pass the case above unchanged. Here the only all-model week is a
	// SURFACE-scoped entry out of limits[], which is walked after the fixed
	// five: the per-model cap is found FIRST and must still lose.
	reversed := &usage.Snapshot{
		FiveHour: elapsedWindow(fiveHourLen, 0.30, 10),
		// The model cap: untouched, nearly elapsed, and reached first.
		SevenDayOpus: elapsedWindow(weekLen, 0.91, 0),
		// Claude Code is itself a surface, so this one binds whatever model
		// runs -- and it is 90% spent with half its week left.
		Limits: []usage.Limit{scoped("", "Claude Code", 90, remaining(weekLen, 0.50))},
	}
	w, stranded, has = hoverStranded(reversed, "", opts().Thresholds(), 2, now, 0, false)
	if kind, ok := usage.ScopeKindOf(w.Name); !has || !ok || kind != usage.ScopeSurface {
		t.Fatalf("priced %s, want the surface-scoped week: reaching a per-model cap first must not make it the perishable window", w.Name)
	}
	if stranded != 0 {
		t.Errorf("stranded = %v, want 0", stranded)
	}
}
