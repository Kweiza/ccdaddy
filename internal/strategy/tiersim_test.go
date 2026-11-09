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

// runTierSim drives Decide over the fleet on two-minute ticks, with the live
// account spending weeklyPerTick points of its week a tick and, where it has
// one, fiveHoursPerWeek times that of its five-hour window.
func runTierSim(t *testing.T, label string, accts []tierAccount, weeklyPerTick float64, ticks int) tierSimResult {
	t.Helper()
	base := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	const tick = 2 * time.Minute
	const week = 7 * 24 * time.Hour
	const five = 5 * time.Hour

	res := tierSimResult{switches: map[string]int{}, unspent: map[string]float64{}, served: map[string]float64{}}
	st := NewState()
	live := accts[0].uuid

	for i := 0; i < ticks; i++ {
		now := base.Add(time.Duration(i) * tick)
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
			st.RecordSwitch(live, now)
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
	base := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	return []tierAccount{
		{uuid: "a-max", short: true, weekUtil: 59, weekAt: base.Add(4*day + 5*time.Hour)},
		{uuid: "b-ultra", weekUtil: 39, weekAt: base.Add(5*day + 10*time.Hour)},
		{uuid: "c-ultra", weekUtil: 36, weekAt: base.Add(5*day + 17*time.Hour)},
		{uuid: "d-ultra", weekUtil: 35, weekAt: base.Add(5*day + 17*time.Hour)},
	}
}

// A week of today's fleet at two loads: light, where a five-hour window takes
// its whole five hours to fill, and heavy, where it fills in two.
//
// Measured on the pace-based hover this harness was landed against, before the
// tier order replaced it. Unspent is weekly quota that reached a weekly
// rollover unspent, per tier; stopped is ticks a limit cut the session off.
//
//	load            switches  of which better-target  unspent short / weekly-only  stopped
//	light 0.02/tick       51                      50          28.50 / 120.30           0
//	heavy 0.05/tick      202                     196           6.00 /  29.30           0
//
// Two hundred moves a week is the cost the owner measured in another way: every
// switch rewrites the prompt cache of a running session. The pace rotation was
// spending that to hold four accounts level with their own clocks.
func TestTodaysFleetOverAWeek(t *testing.T) {
	const ticks = 7 * 24 * 30
	for _, load := range []struct {
		name string
		rate float64
	}{{"light", 0.02}, {"heavy", 0.05}} {
		t.Run(load.name, func(t *testing.T) {
			res := runTierSim(t, fmt.Sprintf("hover, today's fleet, %s load", load.name), todaysFleet(), load.rate, ticks)
			// The harness has to close its own books before any figure it
			// reports can be read: everything served came out of somebody's
			// week, and nothing was served that the load did not ask for.
			served := res.served["short-window"] + res.served["weekly-only"]
			if asked := load.rate * ticks; served > asked+1e-6 {
				t.Errorf("served %.2f of %.2f asked: the harness is inventing work", served, asked)
			}
		})
	}
}
