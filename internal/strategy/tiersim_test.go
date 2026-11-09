package strategy

import (
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
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
//
// It is POLL-FAITHFUL, and that is what makes its figures worth reading. An
// earlier version handed Decide the true state of every account on every tick,
// stamped one minute either side of now, which is a fleet the engine never sees:
// a reading in production is as old as its poll, an account the session has just
// left keeps the rate it was measured at until it is next read, and a rollover
// is invisible until the poll after it. Each of those is a way a rule can look
// right in a simulation and stop a session in the field. So here:
//
//   - the engine sees only what each account's last poll read (tierPoller),
//     and decides at the daemon's own cadence, once a second (asked as
//     tierSimDrift says, which is the same answers);
//   - each account is polled on the schedule the daemon gives it -- the
//     cadence from pollpolicy.Next, behind the serve-TTL floor due() holds,
//     pulled to its next rollover by pollpolicy.WarmClamp -- and FetchedAt and
//     NextPollAt are that schedule's;
//   - the rate is the one the daemon measures: usage.BurnPerMin between two
//     consecutive readings of that account's binding window;
//   - the true state rolls over at its reset, and the reading shows it at the
//     next poll;
//   - every switch is recorded in State, so the cooldown binds.
//
// What it does not model, and why each is safe to leave out here: 429s and the
// stand-down (every account here carries its own organization, so a stand-down
// has no peer to hold back, and nothing is refused); and the warm-up probe, which
// starts a cold window's clock unasked -- the five-hour account's cold window is
// re-entered at the poll that finds it cold, which starts the same clock within
// the same second, and a weekly window here re-anchors at its rollover, as a
// probed one does.

// tierAccount is one account of the simulated fleet: its TRUE state, which the
// engine never reads directly.
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
	// fableAt is when a model-scoped Fable week that is past its last point rolls
	// over, for the variant whose account has one; zero for an account that
	// carries none. The work never spends Fable, which is what a blown cap
	// leaves a session to do, so it stays where it rolled to.
	fableUtil float64
	fableAt   time.Time
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

// read is what a poll of this account returns at this instant.
func (a tierAccount) read() *usage.Snapshot {
	week, weekAt := a.weekUtil, a.weekAt
	s := &usage.Snapshot{SevenDay: usage.NewWindow(&week, &weekAt)}
	if a.short {
		s.FiveHour = usage.NewWindow(nil, nil)
		if !a.fiveAt.IsZero() {
			five, fiveAt := a.fiveUtil, a.fiveAt
			s.FiveHour = usage.NewWindow(&five, &fiveAt)
		}
	}
	if !a.fableAt.IsZero() {
		pct, at := a.fableUtil, a.fableAt
		s.Limits = []usage.Limit{usage.LimitFor(usage.LimitInput{
			Kind: "weekly_scoped", Model: "Fable", Percent: &pct, ResetsAt: &at,
		})}
	}
	return s
}

// roll applies every reset that has come due, returning the weekly quota the
// rollover took away unspent. The reading does not see any of it until the
// account is next polled.
func (a *tierAccount) roll(now time.Time) (unspent float64) {
	const week = 7 * day
	if !now.Before(a.weekAt) {
		unspent = 100 - a.weekUtil
		a.weekUtil, a.weekAt = 0, a.weekAt.Add(week)
	}
	// A five-hour window that has run out its clock is unstarted again until
	// the next turn spent against it.
	if a.short && !a.fiveAt.IsZero() && !now.Before(a.fiveAt) {
		a.fiveUtil, a.fiveAt = 0, time.Time{}
	}
	if !a.fableAt.IsZero() && !now.Before(a.fableAt) {
		a.fableUtil, a.fableAt = 0, a.fableAt.Add(week)
	}
	return unspent
}

// spend takes w weekly points of work, starting the five-hour clock if this is
// the first turn against a cold window.
func (a *tierAccount) spend(w float64, now time.Time) {
	if w <= 0 {
		return
	}
	a.weekUtil += w
	if a.short {
		if a.fiveAt.IsZero() {
			a.fiveAt = now.Add(5 * time.Hour)
		}
		a.fiveUtil += w * fiveHoursPerWeek
	}
}

// tierSimJitter is the sample every jittered range is drawn at: the midpoint,
// which is each cadence exactly as pollpolicy states it and each warm-up target
// halfway through its margin.
const tierSimJitter = 0.5

// tierPoller is what the daemon's cache holds for one account, written the way
// the daemon's commit writes it.
type tierPoller struct {
	snap                  *usage.Snapshot
	fetchedAt, nextPollAt time.Time
	state                 pollpolicy.State
	last                  usage.BindingSample
	burn                  float64
	hasBurn               bool
	burnWindow            usage.WindowName
}

// due is the daemon's due(): never read is due, and otherwise the reading has to
// be older than the serve TTL and its schedule has to have arrived.
func (p *tierPoller) due(now time.Time) bool {
	if p.snap == nil {
		return true
	}
	if now.Sub(p.fetchedAt) < pollpolicy.ServeTTL {
		return false
	}
	return !now.Before(p.nextPollAt)
}

// poll records one reading the way the daemon's commit does: the cadence for the
// next look from the reading and whether the account is live, pulled to the next
// rollover; and the rate, against the previous reading of the same binding
// window.
func (p *tierPoller) poll(s *usage.Snapshot, thr Thresholds, live bool, now time.Time) {
	h := HeadroomOrCredit(s, thr)
	spent, _ := Spent(h)
	reading := pollpolicy.Reading{Exhausted: spent}
	if h.Known {
		reading.BindingPct, reading.Known = 100-h.Pct, true
	}
	in := pollpolicy.Input{Now: now, Active: live, Reading: reading, Threshold: thr.For(h.Binding)}
	at, next := pollpolicy.Next(p.state, in, tierSimJitter)
	natural := now.Add(pollpolicy.Share(at.Sub(now), 1, in))
	// The daemon's warmTarget: the next rollover, a jittered margin after it.
	var warm time.Time
	if reset, ok := NextResetAmong(s, "", thr, now); ok {
		warm = reset.Add(usage.ProbeWakeMargin + time.Duration(tierSimJitter*float64(usage.ProbeWakeMargin)))
	}
	p.nextPollAt = pollpolicy.WarmClamp(natural, warm, now, pollpolicy.ServeTTL)

	if reading.Known {
		reset, _ := s.ResetFor(h.Binding)
		cur := usage.BindingSample{Window: h.Binding, Pct: reading.BindingPct, At: now, Reset: reset}
		if rate, ok := usage.BurnPerMin(p.last, cur); ok {
			p.burn, p.hasBurn, p.burnWindow = rate, true, h.Binding
		}
		p.last = cur
	}
	p.snap, p.fetchedAt, p.state = s, now, next
}

// candidate is the reading the engine is handed, as engineCandidates builds it.
func (p *tierPoller) candidate(uuid string) Candidate {
	c := sub(uuid, p.snap)
	c.FetchedAt, c.NextPollAt = p.fetchedAt, p.nextPollAt
	c.BurnPerMin, c.HasBurn, c.BurnWindow = p.burn, p.hasBurn, p.burnWindow
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
	// stopped counts the seconds in which the live account could not take that
	// second's work, and stoppedWithRoom the ones among them in which some other
	// account could have taken all of it. The second is the one that is a
	// defect: the first also counts a fleet that has simply run out.
	stopped, stoppedWithRoom int
}

// tierSimEpoch is where every tier simulation starts, and the instant
// todaysFleet's resets are measured from.
var tierSimEpoch = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)

// tierSimTick is the daemon's evaluation cadence (internal/daemon's
// tickInterval). It is shorter than the hover cooldown, so the cooldown is a
// real bound here and not one the tick size satisfies on its own.
const tierSimTick = time.Second

// tierSimDrift is how long the harness lets the engine go unasked when nothing
// it reads has changed.
//
// The daemon asks once a second, and asking a week of seconds is what made this
// file take half a minute. Most of those answers cannot differ from the one a
// second before: Decide reads the cached readings, the state and the clock, and
// every change but the clock's is an event the loop already sees -- a reading
// landing, the cooldown ending, a rollover a reading names passing. Each of
// those is asked at its own second (nextAsk). What moves with the clock alone is
// a projection's deadline, a window's average pace and the recovery horizon,
// and this step bounds how late the harness can notice one of them -- at heavy
// load the live five-hour window moves a seventh of a point in it.
//
// Measured once against the harness asking every second (this constant set to
// tierSimTick): every load and fleet below gave the same switches, by the same
// reasons, the same unspent quota to the hundredth, and the same zero stopped
// seconds.
const tierSimDrift = 10 * time.Second

// nextAsk is the next second the engine has to be asked even if no reading
// lands before it: the drift step, or sooner if the cooldown ends or a rollover
// one of the readings names comes first.
func nextAsk(now time.Time, st *State, pollers []tierPoller) time.Time {
	at := now.Add(tierSimDrift)
	if left, cooling := st.CooldownRemaining(now, HoverCooldown); cooling && now.Add(left).Before(at) {
		at = now.Add(left)
	}
	for _, p := range pollers {
		for _, w := range p.snap.AllWindows() {
			if reset, ok := w.Reset(); ok && reset.After(now) && reset.Before(at) {
				at = reset
			}
		}
	}
	return at
}

// tierDecide asks the engine about the fleet as its readings stand, the way the
// daemon's evaluation does, and reports false when there is nothing to ask about
// yet: before the first poll there is no reading, and the daemon acts on none.
func tierDecide(accts []tierAccount, pollers []tierPoller, now time.Time, st *State, live string) (Plan, bool) {
	cands := make([]Candidate, 0, len(accts))
	for j := range accts {
		if pollers[j].snap != nil {
			cands = append(cands, pollers[j].candidate(accts[j].uuid))
		}
	}
	if len(cands) == 0 {
		return Plan{}, false
	}
	return Decide(cands, Options{Now: now, Hover: true}, Config{}, st, live), true
}

// runTierSim drives Decide over the fleet for span, at the daemon's one-second
// cadence (asked as tierSimDrift says), with the live account spending
// weeklyPerMin points of its week a minute and, where it has one,
// fiveHoursPerWeek times that of its five-hour window.
func runTierSim(t *testing.T, label string, accts []tierAccount, weeklyPerMin float64, span time.Duration) tierSimResult {
	t.Helper()
	res := tierSimResult{switches: map[string]int{}, unspent: map[string]float64{}, served: map[string]float64{}}
	st := NewState()
	live := accts[0].uuid
	pollers := make([]tierPoller, len(accts))
	want := weeklyPerMin * tierSimTick.Minutes()
	thr := HoverPlan{}.For
	var askAt time.Time

	for now := tierSimEpoch; now.Before(tierSimEpoch.Add(span)); now = now.Add(tierSimTick) {
		for j := range accts {
			res.unspent[accts[j].tierOf()] += accts[j].roll(now)
		}

		if !now.Before(askAt) {
			if p, asked := tierDecide(accts, pollers, now, st, live); asked {
				if p.Action == ActionSwitch {
					live = p.Target.UUID
					st.RecordSwitch(live, now, p.Departure)
					res.switches[p.Reason.String()]++
					res.total++
				}
				if p.Hover != nil {
					thr = p.Hover.For
				}
				askAt = nextAsk(now, st, pollers)
			}
		}
		// After the decision, as the daemon dispatches its polls: an account
		// that just became live is polled as the live one, and the reading is
		// the engine's to read at the next second.
		for j := range accts {
			if pollers[j].due(now) {
				pollers[j].poll(accts[j].read(), thr(accts[j].uuid), accts[j].uuid == live, now)
				askAt = now.Add(tierSimTick)
			}
		}

		for j := range accts {
			a := &accts[j]
			if a.uuid != live {
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
			a.spend(w, now)
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
	t.Logf("   session-stopping seconds %d, of which another account had room in %d", res.stopped, res.stoppedWithRoom)
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

// fableBlownFleet is today's fleet with the Max account's Fable week past its
// last point, rolling with the rest of its week, while every other model still
// has the whole of the account's room. 99.5 and not 100 is the case that bites:
// a cap with nothing left already stops being the binding window, and one past
// hover's line and not yet empty is the account's tightest window on any table
// that binds it -- so the poll, which schedules and measures its rate through
// the same HeadroomOrCredit the daemon calls, would read a rate of zero off a
// window nothing is spending against.
func fableBlownFleet() []tierAccount {
	f := todaysFleet()
	f[0].fableUtil, f[0].fableAt = 99.5, f[0].weekAt
	return f
}

// A week of today's fleet at three loads: idle, where a five-hour window never
// fills; light, where it takes its whole five hours to fill; heavy, where it
// fills in two. Each runs on today's fleet and on the same fleet with the Max
// account's Fable week past its last point. Unspent is weekly quota that reached a weekly
// rollover unspent, per tier; stopped is seconds a limit cut the session off.
//
// Measured with this harness on both codes: the pace rotation hover ran before,
// on the commit this branch was cut from, and the order. Load is weekly points a
// minute; unspent is short-window / weekly-only; stopped is in seconds. The pace
// rotation was asked every second (tierSimDrift at tierSimTick), because the
// thresholds it derived moved with the clock and the drift measurement above was
// taken on the order.
//
//	                          pace rotation                    the order
//	load           fleet   switches  unspent         stopped   switches  unspent         stopped
//	idle  0.0075   today         43  31.78 / 137.59        0          2  0.98 / 184.56         0
//	idle  0.0075   Fable         29  41.00 / 128.35        0          2  0.98 / 184.56         0
//	light 0.01     today         52  28.71 / 120.11        0         54  0.99 / 168.44         0
//	light 0.01     Fable         33  41.00 / 107.80        0         54  0.99 / 168.44         0
//	heavy 0.025    today        210   6.04 /  28.79        0         59  0.97 /  45.15         0
//	heavy 0.025    Fable        165  33.46 /   9.22      484         59  0.97 /  45.15         0
//
// Two hundred moves a week was the cost the owner measured another way: every
// switch rewrites the prompt cache of a running session, and the rotation spent
// that to hold four accounts level with their own clocks. The order moves the
// session twice per five-hour window it fills -- out before the limit, back at
// the rollover -- and not otherwise, which is why light load, whose window fills
// in the last minutes of its five hours, costs as many moves as heavy load does.
//
// The Fable week past its last point is the variant the rotation failed
// outright: it read the cap as the Max account's limit, left most of the
// account's week unspent, and stopped the session for 484 seconds while other
// accounts had room. Under the order a cap on one model family is not a limit,
// and the week is the same week to the hundredth.
//
// What it costs is the weekly-only seats' quota, stated plainly: they are spent
// only while the five-hour account is at a limit, and then only the one whose
// week ends first, so most of three Ultra weeks now expire unspent. What it
// buys is the five-hour account spent to its last point every week.
func TestTodaysFleetOverAWeek(t *testing.T) {
	const week = 7 * day
	const fiveHourWindows = float64(week) / float64(5*time.Hour)
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
		{"idle", 0.0075, 2 + 4},
		{"light", 0.01, 2*fiveHourWindows + 4},
		{"heavy", 0.025, 2*fiveHourWindows + 4},
	} {
		for _, fleet := range []struct {
			name string
			of   func() []tierAccount
		}{
			{"today", todaysFleet},
			{"Fable blown", fableBlownFleet},
		} {
			load, fleet := load, fleet
			t.Run(load.name+"/"+fleet.name, func(t *testing.T) {
				t.Parallel()
				res := runTierSim(t, fmt.Sprintf("hover, %s fleet, %s load", fleet.name, load.name), fleet.of(), load.rate, week)
				// The harness has to close its own books before any figure it
				// reports can be read: everything served came out of somebody's
				// week, and nothing was served that the load did not ask for.
				served := res.served["short-window"] + res.served["weekly-only"]
				if asked := load.rate * week.Minutes(); served > asked+1e-6 {
					t.Errorf("served %.2f of %.2f asked: the harness is inventing work", served, asked)
				}

				// Purpose 2, and it is absolute: no limit stops the session while
				// another account could have carried it. This fleet never runs
				// dry inside the week, so no second may stop at all.
				if res.stoppedWithRoom != 0 || res.stopped != 0 {
					t.Errorf("%d seconds stopped by a limit, %d of them with another account holding room",
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
}
