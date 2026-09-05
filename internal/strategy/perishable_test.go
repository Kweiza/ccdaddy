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
				_, stranded, _ := hoverStranded(s, "", opts().Thresholds(), usable, now)
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
	if _, stranded, _ := hoverStranded(s, "", opts().Thresholds(), 4, now); stranded != 0 {
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

	_, stranded, has := hoverStranded(s, "", opts().Thresholds(), 4, now)
	if has || stranded != 0 {
		t.Errorf("stranded = %v (has %v), want 0: the window has already rolled", stranded, has)
	}
}

// The cap and the empty tier read the SAME room, and this is the pair that stops
// the second spelling drifting. hoverRoom is OutOfQuota's rule as a number, so
// it must go to zero at exactly the instant OutOfQuota files the account empty
// -- otherwise a blown model-scoped cap comes to zero a licence that OutOfQuota
// would not zero.
func TestTheStrandedCapAndTheEmptyTierReadTheSameRoom(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *usage.Snapshot
	}{
		{"room in both", snap(win(10, time.Hour), win(20, weekLen/2))},
		{"the week is empty", snap(win(10, time.Hour), win(100, weekLen/2))},
		{"the five-hour window is empty", snap(win(100, time.Hour), win(20, weekLen/2))},
		{"a blown sub-cap beside a week with room", &usage.Snapshot{
			FiveHour:     win(10, time.Hour),
			SevenDay:     win(20, weekLen/2),
			SevenDayOpus: win(100, weekLen/2),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := opts().Thresholds()
			room := hoverRoom(tc.s, "", th)
			empty, known := OutOfQuota(HeadroomFor(tc.s, "", th))
			if !known {
				t.Fatal("OutOfQuota could not answer for a readable snapshot")
			}
			if (room <= 0) != empty {
				t.Errorf("hoverRoom = %v but OutOfQuota says empty=%v: the two spellings have drifted", room, empty)
			}
		})
	}
}

// ptr is a pointer to a value, for the windows that state a reset instant
// directly rather than as a share of a length.
func ptr[T any](v T) *T { return &v }

// The hole the room cap closes, which is the one way this term could hand the
// session to an account that cannot serve it.
//
// An account whose five-hour window is nearly spent still holds a whole week
// that expires in fifteen hours. Priced on the week alone its licence is 81
// points and its five-hour pace target 171, which puts it AHEAD of an account
// holding sixty -- and Decide then switches onto an account with ten points of
// room. The cap is what says a licence is a claim on the next session, so it
// may not exceed what the account could serve.
func TestANearlyEmptyAccountCannotBuyTheFrontOfTheQueue(t *testing.T) {
	// Ten points of five-hour room, and a week 91% elapsed at 1% used: 99
	// points that nothing but this account can reach, and it cannot reach them
	// either.
	nearly := sub("a-nearly-empty", &usage.Snapshot{
		FiveHour: elapsedWindow(fiveHourLen, 0.90, 90),
		SevenDay: elapsedWindow(weekLen, 0.91, 1),
	})
	// Sixty points of week left and half the week to spend them in, so its
	// rotation reaches all of it and nothing strands.
	roomy := sub("b-roomy", &usage.Snapshot{
		FiveHour: elapsedWindow(fiveHourLen, 0.70, 10),
		SevenDay: elapsedWindow(weekLen, 0.50, 40),
	})
	pool := []Candidate{nearly, roomy}

	p := HoverThresholds(pool, hoverOpts())
	a, ok := p.AccountFor("a-nearly-empty")
	if !ok || a.Stranded != 10 {
		t.Fatalf("stranded = %v, want the 10 points of room the account actually has", a.Stranded)
	}
	eq(t, order(Rank(pool, hoverOpts())), []string{"b-roomy", "a-nearly-empty"})

	if d := Decide(pool, hoverOpts(), Config{}, NewState(), "b-roomy"); d.Action != ActionStay {
		t.Errorf("Action = %v onto %s, want a stay: it has ten points left", d.Action, d.Target.UUID)
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

	w, stranded, has := hoverStranded(s, "", opts().Thresholds(), 2, now)
	if !has || w.Name != usage.WindowSevenDay {
		t.Fatalf("priced %s, want seven_day: a cap on one model family cannot be the perishable window while an all-model week is readable", w.Name)
	}
	if stranded != 0 {
		t.Errorf("stranded = %v, want 0: the week the session actually spends is 90 percent gone", stranded)
	}
}
