package strategy

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The tier simulation: a mixed fleet driven through Decide for a whole week,
// measured on the three things the owner judges hover by -- how often it moves,
// how much weekly quota reaches a reset unspent, and whether a session is ever
// stopped by a limit while another account still had room.
//
// It differs from runWasteSim in the two ways today's fleet differs from the
// one that harness was written for. Some accounts carry no five-hour window at
// all: a verified Ultra seat reads five_hour null, and the week is the only
// limit it has. And a five-hour window is ANCHORED, the way the endpoint
// anchors it: it reads {null,null} until a turn is spent against it, and that
// turn starts a five-hour clock. A window on a fixed cohort would hand an idle
// account a fresh window it never asked for, which is the one thing the return
// to a five-hour account has to wait on.

// tierAccount is one account of the simulated fleet.
type tierAccount struct {
	uuid string
	// short is whether the account carries a five-hour window at all.
	short bool
	// weekUtil and weekAt are the weekly window and when it next rolls over.
	weekUtil float64
	weekAt   time.Time
	// fiveUtil and fiveAt are the five-hour window. A zero fiveAt is a window
	// nothing has started, which the reading carries as {null,null}.
	fiveUtil float64
	fiveAt   time.Time
	// burn is what the last tick spent here, in points of burnWindow a minute:
	// the figure the poller would have measured between two readings.
	burn       float64
	burnWindow usage.WindowName
}

// tierOf names the account's tier the way the owner does.
func (a tierAccount) tierOf() string {
	if a.short {
		return "short-window"
	}
	return "weekly-only"
}

// fiveRoom is how many weekly points the five-hour window still lets through. A
// window nothing has started lets a whole one through.
func (a tierAccount) fiveRoom() float64 {
	if !a.short {
		return math.Inf(1)
	}
	return (100 - a.fiveUtil) / fiveHoursPerWeek
}

// canServe is how many of want weekly points this account can take right now:
// both of its windows have to have room for the same work.
func (a tierAccount) canServe(want float64) float64 {
	return math.Max(0, math.Min(want, math.Min(100-a.weekUtil, a.fiveRoom())))
}

// candidate is the reading the poller would hand the engine for this account.
func (a tierAccount) candidate(now time.Time) Candidate {
	week, weekAt := a.weekUtil, a.weekAt
	s := &usage.Snapshot{SevenDay: usage.NewWindow(&week, &weekAt)}
	if a.short {
		s.FiveHour = usage.NewWindow(nil, nil)
		if !a.fiveAt.IsZero() {
			five, fiveAt := a.fiveUtil, a.fiveAt
			s.FiveHour = usage.NewWindow(&five, &fiveAt)
		}
	}
	c := sub(a.uuid, s)
	c.FetchedAt, c.NextPollAt = now.Add(-time.Minute), now.Add(time.Minute)
	c.BurnPerMin, c.HasBurn, c.BurnWindow = a.burn, true, a.burnWindow
	return c
}

// tierSimResult is what one run measured.
type tierSimResult struct {
	// switches counts every move, by the reason the plan gave for it.
	switches map[string]int
	total    int
	// unspent is the weekly quota that reached a weekly rollover unspent, and
	// served what the tier actually absorbed, both summed per tier.
	unspent, served map[string]float64
	// stopped counts the ticks on which the live account could not take the
	// tick's work, and stoppedWithRoom the ones among them on which some other
	// account could have taken all of it. The second is the one that is a
	// defect: the first also counts a fleet that has simply run out.
	stopped, stoppedWithRoom int
}

// tierSimEpoch is where every tier simulation starts, and the instant
// todaysFleet's resets are measured from.
var tierSimEpoch = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

// runTierSim drives Decide over the fleet on two-minute ticks, with the live
// account spending weeklyPerTick points of its week a tick and, where it has
// one, fiveHoursPerWeek times that of its five-hour window.
func runTierSim(t *testing.T, label string, accts []tierAccount, weeklyPerTick float64, ticks int) tierSimResult {
	t.Helper()
	const tick = 2 * time.Minute
	const week = 7 * day
	const five = 5 * time.Hour

	res := tierSimResult{switches: map[string]int{}, unspent: map[string]float64{}, served: map[string]float64{}}
	st := NewState()
	live := accts[0].uuid

	for i := 0; i < ticks; i++ {
		now := tierSimEpoch.Add(time.Duration(i) * tick)
		for j := range accts {
			a := &accts[j]
			// A weekly rollover is where the accounting happens: whatever the
			// window still held is gone, and no later tick can spend it.
			if !now.Before(a.weekAt) {
				res.unspent[a.tierOf()] += 100 - a.weekUtil
				a.weekUtil, a.weekAt = 0, a.weekAt.Add(week)
			}
			// A five-hour window that has run out its clock is unstarted again
			// until the next turn spent against it.
			if a.short && !a.fiveAt.IsZero() && !now.Before(a.fiveAt) {
				a.fiveUtil, a.fiveAt = 0, time.Time{}
			}
		}

		cands := make([]Candidate, 0, len(accts))
		for _, a := range accts {
			cands = append(cands, a.candidate(now))
		}
		p := Decide(cands, Options{Now: now, Hover: true}, Config{}, st, live)
		if p.Action == ActionSwitch {
			live = p.Target.UUID
			st.RecordSwitch(live, now, p.Departure)
			res.switches[p.Reason.String()]++
			res.total++
		}

		want := weeklyPerTick
		for j := range accts {
			a := &accts[j]
			if a.uuid != live {
				a.burn = 0
				continue
			}
			w := a.canServe(want)
			if w < want {
				res.stopped++
				for _, o := range accts {
					if o.uuid != live && o.canServe(want) >= want {
						res.stoppedWithRoom++
						break
					}
				}
			}
			a.weekUtil += w
			a.burn, a.burnWindow = w/tick.Minutes(), usage.WindowSevenDay
			if a.short && w > 0 {
				if a.fiveAt.IsZero() {
					a.fiveAt = now.Add(five)
				}
				a.fiveUtil += w * fiveHoursPerWeek
				a.burn, a.burnWindow = w*fiveHoursPerWeek/tick.Minutes(), usage.WindowFiveHour
			}
			res.served[a.tierOf()] += w
		}
	}

	t.Logf("%s: %d switches", label, res.total)
	reasons := make([]string, 0, len(res.switches))
	for r := range res.switches {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		t.Logf("   %4d  %s", res.switches[r], r)
	}
	for _, tier := range []string{"short-window", "weekly-only"} {
		t.Logf("   %-12s served %7.2f  unspent at weekly rollovers %7.2f", tier, res.served[tier], res.unspent[tier])
	}
	t.Logf("   session-stopping ticks %d, of which another account had room on %d", res.stopped, res.stoppedWithRoom)
	return res
}

// todaysFleet is the fleet of 2026-11-09: one Max account carrying a five-hour
// window, and three verified Ultra seats that carry only a week.
func todaysFleet() []tierAccount {
	return []tierAccount{
		{uuid: "a-max", short: true, weekUtil: 59, weekAt: tierSimEpoch.Add(4*day + 5*time.Hour)},
		{uuid: "b-ultra", weekUtil: 39, weekAt: tierSimEpoch.Add(5*day + 10*time.Hour)},
		{uuid: "c-ultra", weekUtil: 36, weekAt: tierSimEpoch.Add(5*day + 17*time.Hour)},
		{uuid: "d-ultra", weekUtil: 35, weekAt: tierSimEpoch.Add(5*day + 17*time.Hour)},
	}
}

// A week of today's fleet at three loads: idle, where a five-hour window never
// fills; light, where it takes its whole five hours to fill; heavy, where it
// fills in two. Unspent is weekly quota that reached a weekly rollover unspent,
// per tier; stopped is ticks a limit cut the session off.
//
// The pace rotation hover ran before -- measured on the code this harness was
// landed against, the idle row afterwards from the same commit:
//
//	load            switches  of which better-target  unspent short / weekly-only  stopped
//	idle  0.015/t         43                      43          31.79 / 137.56           0
//	light 0.02/t          51                      50          28.50 / 120.30           0
//	heavy 0.05/t         202                     196           6.00 /  29.30           0
//
// The tier order:
//
//	load            switches  limit hand-offs  returns   unspent short / weekly-only  stopped
//	idle  0.015/t          2                1        1            0.99 / 184.55           0
//	light 0.02/t          54               27       27            0.98 / 169.00           0
//	heavy 0.05/t          57               30       27            0.95 /  46.35           0
//
// Two hundred moves a week was the cost the owner measured another way: every
// switch rewrites the prompt cache of a running session, and the rotation spent
// that to hold four accounts level with their own clocks. The order moves the
// session twice per five-hour window it fills -- out at the limit, back at the
// rollover -- and not otherwise, which is why light load, whose window fills in
// the last minutes of its five hours, costs as many moves as heavy load does.
//
// What it costs is the weekly-only seats' quota, stated plainly: they are spent
// only while the five-hour account is at a limit, and then only the one whose
// week ends first, so most of three Ultra weeks now expire unspent. What it
// buys is the five-hour account spent to its last point every week.
func TestTodaysFleetOverAWeek(t *testing.T) {
	const ticks = 7 * 24 * 30
	const fiveHourWindows = float64(7*day) / float64(5*time.Hour)
	for _, load := range []struct {
		name string
		rate float64
		// most is the switch bound: two per five-hour window the load fills
		// -- the hand-off and the return -- and one per weekly rollover in the
		// fleet, which is the only other event that reorders a seat. A load
		// that never fills a window leaves only the rollovers and the week's
		// own last point.
		most float64
	}{
		{"idle", 0.015, 2 + 4},
		{"light", 0.02, 2*fiveHourWindows + 4},
		{"heavy", 0.05, 2*fiveHourWindows + 4},
	} {
		t.Run(load.name, func(t *testing.T) {
			res := runTierSim(t, fmt.Sprintf("hover, today's fleet, %s load", load.name), todaysFleet(), load.rate, ticks)
			// The harness has to close its own books before any figure it
			// reports can be read: everything served came out of somebody's
			// week, and nothing was served that the load did not ask for.
			served := res.served["short-window"] + res.served["weekly-only"]
			if asked := load.rate * ticks; served > asked+1e-6 {
				t.Errorf("served %.2f of %.2f asked: the harness is inventing work", served, asked)
			}

			// Purpose 2, and it is absolute: no limit stops the session while
			// another account could have carried it. This fleet never runs dry
			// inside the week, so no tick may stop at all.
			if res.stoppedWithRoom != 0 || res.stopped != 0 {
				t.Errorf("%d ticks stopped by a limit, %d of them with another account holding room",
					res.stopped, res.stoppedWithRoom)
			}
			if float64(res.total) > load.most {
				t.Errorf("%d switches in a week, want at most %.0f: the order is moving the session "+
					"for something other than a limit, a rollover or a return", res.total, load.most)
			}
			// The five-hour tier is spent to its last point before its week
			// ends: one account, one rollover, at most the one point the
			// hand-off line keeps back.
			if got := res.unspent["short-window"]; got > handoffRoomPct {
				t.Errorf("%.2f points of the five-hour account's week expired unspent, want at most %v", got, handoffRoomPct)
			}
		})
	}
}
