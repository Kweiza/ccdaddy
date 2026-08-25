package forecast

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

const (
	// horizon is how far a run looks before it stops and reports that the axis
	// holds.
	//
	// Twice the longest window this release knows a length for, so that a fleet
	// whose weekly resets are staggered across the week gets a full cycle of
	// every one of them and then a second one. A horizon of exactly one week
	// would end the run in the middle of the cycle it is trying to judge, and a
	// fleet judged over half a cycle reports whichever half it was shown.
	//
	// It is also what makes the run finite in the one direction that is not
	// already bounded. Every step either rolls a window over or spends an
	// account, accounts are spent at most once between two rollovers, and the
	// number of rollovers is this figure divided by the shortest window's
	// length -- 67 per window on the five-hour axis.
	horizon = 14 * 24 * time.Hour

	// horizonHours is the horizon in the units the rates are measured in, so an
	// event can be rejected as out of range BEFORE it becomes a time.Duration.
	//
	// The conversion is where the damage would be. An event computed against a
	// rate of zero is an infinite number of hours, and time.Duration(inf) is not
	// a saturated maximum -- it is a large NEGATIVE count on the platforms this
	// ships to. An out-of-range event would not park the clock past the end of
	// the run, it would run the clock backwards.
	horizonHours = float64(horizon) / float64(time.Hour)
)

// simWindow is one window of one account as the run models it: the level read
// from the current snapshot, the rollover that reading reported, and how long
// the window runs.
//
// A zero reset means the reading carried none. That window still burns; it
// simply never rolls over, so it can only shorten a runway. A length of zero
// means this release knows no length for the window, which is cinder_cove's
// case -- its resets_at is an expiry rather than a rollover, and rolling it over
// would invent an endless series of grants that never arrive.
type simWindow struct {
	pct    float64
	reset  time.Time
	length time.Duration
}

// simAccount is one account as the run models it. Idx is carried because it
// breaks ties in the choice of live account: two accounts with identical room
// must be picked in the same order on every run, or six runs of one fleet
// disagree about a fleet that never changed.
type simAccount struct {
	uuid    string
	idx     int
	windows map[usage.WindowName]simWindow
}

// readable reports whether any window of this account reported a percentage.
//
// An account with none is unreadable and is never chosen as live. Fail closed:
// a runway that counts an account nobody can see is a promise the fleet may not
// be able to keep.
func (a *simAccount) readable() bool { return len(a.windows) > 0 }

// minRoom is the least room -- 100 minus the utilization -- across ALL of this
// account's windows, whether or not they are in the run's scope.
//
// Room, not slack, and the difference is not cosmetic. strategy.Headroom
// carries both: its Pct comes off the window with the least SLACK, which is
// threshold-derived, and hover rewrites thresholds every tick. MinPct's own doc
// in internal/strategy/headroom.go works an ordinary pair through: a five-hour
// window a quarter used reports 75 points while the weekly window behind it, at
// 95%, has 5 -- and it is the 5 that says how much work the account can still
// take. No strategy.Thresholds value reaches this package at all, which is what
// keeps a forecast from moving because a ranking moved.
//
// An account with no windows answers 100 -- the identity of a minimum over
// rooms -- rather than 0. Zero would quietly do readable's job in the one place
// it must not: an unreadable account is excluded because nobody can see it, not
// because it is spent, and folding one reason into the other hides which of
// them applied.
func (a *simAccount) minRoom() float64 {
	room := 100.0
	for _, w := range a.windows {
		if r := 100 - w.pct; r < room {
			room = r
		}
	}
	return room
}

// usable is BOTH readable and not out, and the second half is what keeps this
// run finite.
//
// Readability alone is not enough. An account at 100% is perfectly readable, so
// a step that picks it finds its earliest window already at the limit, computes
// a zero-length interval and advances no clock -- forever. That is not a corner
// case: a fleet with every account spent is the designed end state of this
// rotation, which internal/strategy/drain_test.go exists to assert.
//
// Out is measured over every window, not over the run's scope. An account whose
// weekly window is at 100 cannot be switched to on any axis, so a five-hour run
// that handed it work would count capacity the fleet cannot reach.
func (a *simAccount) usable() bool { return a.readable() && a.minRoom() > 0 }

// simulate runs the rotation with every window in scope. It is the fleet
// answer: both axes at once, which is the one a user is asking about when they
// ask how long the fleet lasts.
func simulate(accounts []simAccount, rates map[usage.WindowName]float64, now time.Time) (time.Time, bool, map[string]time.Time) {
	seen := make(map[usage.WindowName]bool)
	scope := make([]usage.WindowName, 0, len(rates))
	for i := range accounts {
		for n := range accounts[i].windows {
			if !seen[n] {
				seen[n] = true
				scope = append(scope, n)
			}
		}
	}
	return simulateScoped(accounts, rates, scope, now)
}

// simulateScoped runs the rotation with only the named windows burning and only
// they ending the run.
//
// dryAt and dry are the fleet's answer: the moment no account can take work and
// none recovers before the horizon. emptyAt names, per account uuid, the first
// moment the run saw that account out -- now, for one that starts out. An
// unreadable account never appears there: it ran out of nothing, and naming a
// moment for it would report a fact about a fleet that was never read.
//
// Scope decides what burns; usability is fleet-wide in both entry points.
func simulateScoped(accounts []simAccount, rates map[usage.WindowName]float64, scope []usage.WindowName, now time.Time) (time.Time, bool, map[string]time.Time) {
	// The run spends its own copy. Three scopes at both ends of the measured
	// band are six runs over one fleet, and a run that burned its argument would
	// hand the next one a fleet the previous one had already emptied.
	state := cloneAccounts(accounts)
	inScope := make(map[usage.WindowName]bool, len(scope))
	for _, n := range scope {
		inScope[n] = true
	}
	deadline := now.Add(horizon)
	emptyAt := make(map[string]time.Time, len(state))

	catchUpResets(state, now)
	t := now
	recordEmpty(state, emptyAt, t)

	for !t.After(deadline) {
		live, ok := chooseLive(state)
		if !ok {
			// Nobody can take work. This is not the answer yet: a fleet with
			// every account out is the ordinary end state of a rotation, and on
			// the five-hour axis it clears within five hours. Reporting it as
			// the runway would answer a question about this minute rather than
			// about whether the rate is sustainable.
			//
			// So walk the rollovers until one of them puts an account back in
			// service. Rollovers that do not are walked THROUGH rather than
			// stopped at, because a five-hour window rolling over on an account
			// whose weekly cap is blown changes nothing and there is one of
			// those every five hours: stopping at the first rollover of any kind
			// would report "holds" for any fleet at all, so long as one
			// five-hour reset was readable.
			stalled := t
			for {
				next, has := nextRollover(state, inScope, t, deadline)
				if !has {
					return stalled, true, emptyAt
				}
				t = next
				rollDue(state, inScope, t)
				if _, back := chooseLive(state); back {
					break
				}
			}
			continue
		}

		dt, end, has := nextEvent(state, live, rates, inScope, t, deadline)
		if !has {
			// Nothing burns and nothing rolls over inside the horizon, so the
			// state at the deadline is the state now. Walking there one step at
			// a time would produce the same answer more slowly.
			break
		}
		t = t.Add(dt)
		spend(&state[live], rates, inScope, dt, end)
		rollDue(state, inScope, t)
		recordEmpty(state, emptyAt, t)
	}
	return time.Time{}, false, emptyAt
}

// cloneAccounts copies the fleet deeply enough to burn: the window maps are
// shared by a shallow copy, and a run writes to them on every step.
func cloneAccounts(in []simAccount) []simAccount {
	out := make([]simAccount, len(in))
	for i, a := range in {
		out[i] = simAccount{uuid: a.uuid, idx: a.idx, windows: make(map[usage.WindowName]simWindow, len(a.windows))}
		for n, w := range a.windows {
			out[i].windows[n] = w
		}
	}
	return out
}

// catchUpResets moves a reset that is already in the past forward to the
// window's next real boundary.
//
// The reading the run starts from can be older than the event it describes:
// ColdWindow in internal/strategy/headroom.go carries an arm for exactly this
// case, where "the window ran down; the cached reading is simply older than the
// event". Rollovers fire strictly in the run's future, so a reset left in the
// past would never fire at all, and a periodic quota would be modelled as a
// one-shot balance that never refills.
//
// The utilization is deliberately NOT reset to zero here. The schedule is
// inferred; the level is what was actually read, and handing the fleet a full
// window nobody observed is fail-open on the one figure this package refuses to
// guess at.
func catchUpResets(state []simAccount, now time.Time) {
	for i := range state {
		for n, w := range state[i].windows {
			if w.length <= 0 || w.reset.IsZero() || w.reset.After(now) {
				continue
			}
			for !w.reset.After(now) {
				w.reset = w.reset.Add(w.length)
			}
			state[i].windows[n] = w
		}
	}
}

// chooseLive picks the account that takes the work: the usable one with the
// most room, ties broken by ascending Idx.
//
// Ranking on room is close to what strategy.Rank does and deliberately does not
// import it. The choice barely moves the weekly answer -- total weekly capacity
// is what it is, whoever spends it -- and it moves the five-hour answer only
// through how much five-hour capacity is left unused. What matters more is that
// a forecast must not change because a ranking changed.
func chooseLive(state []simAccount) (int, bool) {
	best, found := -1, false
	var bestRoom float64
	for i := range state {
		a := &state[i]
		if !a.usable() {
			continue
		}
		room := a.minRoom()
		if !found || room > bestRoom || (room == bestRoom && a.idx < state[best].idx) {
			best, bestRoom, found = i, room, true
		}
	}
	return best, found
}

// nextEvent is the interval to the next thing that changes the fleet: the live
// account's earliest in-scope window reaching 100 at that window name's fleet
// rate, or the next rollover among the windows in scope, whichever is sooner.
//
// end names the window that reached 100, and is empty when a rollover won.
//
// A window whose rate is zero contributes NO event. It never reaches 100, and
// the alternative -- an infinite interval -- does not stay out of the way: it
// converts to a large negative time.Duration and runs the clock backwards. The
// same bound catches a rate so small that the event lands past the horizon,
// where the answer is the same either way, and a percentage that is not a
// number, which fails every comparison and lands here too.
func nextEvent(state []simAccount, live int, rates map[usage.WindowName]float64, inScope map[usage.WindowName]bool, t, deadline time.Time) (time.Duration, usage.WindowName, bool) {
	var (
		dt    time.Duration
		end   usage.WindowName
		found bool
	)
	for n, w := range state[live].windows {
		if !inScope[n] {
			continue
		}
		rate := rates[n]
		if rate <= 0 {
			continue
		}
		hours := (100 - w.pct) / rate
		if !(hours >= 0 && hours <= horizonHours) {
			continue
		}
		d := time.Duration(hours * float64(time.Hour))
		// The tie goes to the earlier window name only through <; an exact tie
		// keeps the first one found, and both are at 100 by the end of the
		// interval either way.
		if !found || d < dt {
			dt, end, found = d, n, true
		}
	}
	if next, has := nextRollover(state, inScope, t, deadline); has {
		if d := next.Sub(t); !found || d < dt {
			// A rollover ends the interval without ending an account, so no
			// window is named: naming one here would set it to exactly 100 and
			// spend quota the interval did not reach.
			return d, "", true
		}
	}
	return dt, end, found
}

// nextRollover is the earliest reset strictly after `after` and no later than
// the deadline, across every account's in-scope windows.
//
// Rollovers are looked for across the whole fleet, not just the live account:
// a window's clock runs whether or not anyone is spending against it.
func nextRollover(state []simAccount, inScope map[usage.WindowName]bool, after, deadline time.Time) (time.Time, bool) {
	var out time.Time
	found := false
	for i := range state {
		for n, w := range state[i].windows {
			if !inScope[n] || w.length <= 0 || w.reset.IsZero() {
				continue
			}
			if !w.reset.After(after) || w.reset.After(deadline) {
				continue
			}
			if !found || w.reset.Before(out) {
				out, found = w.reset, true
			}
		}
	}
	return out, found
}

// rollDue rolls over every in-scope window whose reset has arrived: the level
// goes to zero and the reset advances by whole lengths until it is in the
// future again.
//
// Whole lengths rather than one, because an interval can span several: a weekly
// axis stepping past a five-hour window crosses eight of its boundaries at once,
// and advancing by a single length would leave the window rolling over
// repeatedly in the past.
func rollDue(state []simAccount, inScope map[usage.WindowName]bool, t time.Time) {
	for i := range state {
		for n, w := range state[i].windows {
			if !inScope[n] || w.length <= 0 || w.reset.IsZero() || w.reset.After(t) {
				continue
			}
			w.pct = 0
			for !w.reset.After(t) {
				w.reset = w.reset.Add(w.length)
			}
			state[i].windows[n] = w
		}
	}
}

// spend applies one interval of burn to the live account's in-scope windows,
// each at its own window name's fleet rate.
//
// One account, because ccdad has one live login at a time. Spreading the fleet
// rate across every account in parallel reports a dry moment roughly n times too
// early, which on a six-account fleet is the difference between an hour and an
// evening.
//
// end is set to exactly 100 rather than left to the arithmetic. Multiplying a
// rate by an interval derived from that same rate does not land on 100; it lands
// within a few parts in 10^16, and a residue that size leaves the account usable
// with a room of 10^-14, makes the next interval 10^-16 hours long, and the run
// crawls forward in steps too small to see instead of finishing. The clamp is
// the same defence for the windows that did not end the interval.
func spend(a *simAccount, rates map[usage.WindowName]float64, inScope map[usage.WindowName]bool, dt time.Duration, end usage.WindowName) {
	hours := dt.Hours()
	for n, w := range a.windows {
		if !inScope[n] {
			continue
		}
		if rate := rates[n]; rate > 0 {
			w.pct += rate * hours
		}
		if n == end || w.pct > 100 {
			w.pct = 100
		}
		a.windows[n] = w
	}
}

// recordEmpty notes the first moment the run saw each account out.
//
// First, and never overwritten: an account that empties, rolls over and empties
// again is reported at the earlier moment, because that is when a user first
// could not reach it. An unreadable account is skipped -- it ran out of nothing,
// and it is already excluded from every choice by usable.
func recordEmpty(state []simAccount, emptyAt map[string]time.Time, t time.Time) {
	for i := range state {
		a := &state[i]
		if !a.readable() || a.minRoom() > 0 {
			continue
		}
		if _, seen := emptyAt[a.uuid]; !seen {
			emptyAt[a.uuid] = t
		}
	}
}
