package strategy

import (
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Headroom is how much of an account's BINDING window is left, and how far that
// window is from the threshold it was given.
//
// It is deliberately not one window's number. An account carries a five-hour
// window and several weekly ones, and the one that binds is whichever is
// closest to its own threshold: ranking on five_hour alone hands work to an
// account whose weekly Opus quota is gone, and it hits a hard limit one prompt
// later.
type Headroom struct {
	// Pct is 100 minus the binding window's utilization. It can go negative,
	// and is kept that way: how far past its limit an account already is still
	// orders it against other spent accounts. This is the DISPLAY axis, the one
	// `ccdad list`'s LEFT column prints, and it asks the same question of every
	// window.
	Pct float64
	// Slack is the binding window's threshold minus its utilization, and it is
	// the axis the ranking orders on.
	//
	// It stops agreeing with Pct the moment two windows carry different
	// thresholds. An account at 55% of a weekly window whose threshold is 60 has
	// 45 points of raw headroom and 5 points of slack; five is the distance that
	// decides whether the next prompt trips a limit, and forty-five is the
	// number that would have ranked it first.
	Slack float64
	// Threshold is the number Slack was measured against.
	//
	// It is carried rather than recovered as Slack+100-Pct: that identity is
	// exact in arithmetic and not in float64, and this figure is printed. It is
	// also the only way a consumer holding a Headroom alone can report the
	// threshold — `ccdad auto --json` renders a ranked pool and never sees the
	// bundle the pass was run with.
	Threshold float64
	// MinPct is the LEAST raw room any binding window has, and MinWindow names
	// it. It is a SEPARATE figure from Pct and it has to be.
	//
	// Pct is read off the window with the least SLACK, and once windows carry
	// thresholds of their own those are two different windows. A five-hour
	// window three percent into its cycle sits under a threshold of twenty and
	// binds at 25% used, while the weekly one behind it is at 95% under a
	// threshold of ninety-nine: Pct reports the five-hour window's 75 points
	// while the weekly is what will actually stop the session eight prompts
	// later. Slack is the right answer to "which window is this account closest
	// to breaching"; it is the wrong answer to "how much work can this account
	// still take", and this field is the second question asked in its own
	// terms.
	MinPct    float64
	MinWindow usage.WindowName
	// Known is false when no window reported a utilization. An account that
	// could not be read is NOT an empty one, and treating it as one is the
	// exact bug that parked cswap's engine permanently.
	Known bool
	// Binding names the window Pct, Slack and Threshold all came from: the one
	// with the least slack. It is the ORDERING axis and the spent test, and the
	// three figures describe ONE window deliberately, so that Slack is always
	// Threshold minus that window's utilization.
	Binding usage.WindowName
	// Floor names the WEEKLY window that has to clear before this account is
	// usable again, and HasFloor is whether there is one at all. Two kinds
	// qualify — one past the number it was given, and one with nothing left in
	// it — and an empty one wins over a merely-tripped one. HeadroomFor's own
	// loop spells out why it takes both.
	//
	// It is the REPORTING axis, and it is separate from Binding because it
	// answers a different question. Binding is what is tightest right now; Floor
	// is what will still be tight in two days. An account whose five-hour window
	// rolls over in eight minutes has not recovered while its weekly cap is
	// blown until Friday, so a tripped weekly window is what a user has to be
	// told about and what has to clear before the account is usable again. It
	// never participates in ordering.
	Floor    usage.WindowName
	HasFloor bool
	// FloorSlack and FloorThreshold are the FLOOR window's own pair: the
	// threshold that window was given, and that threshold minus its
	// utilization. They mean nothing when HasFloor is false and are left at
	// zero there, exactly as Floor itself is.
	//
	// They are here because Slack and Threshold above are assigned off BINDING
	// in one statement, and the moment there is a floor those two names
	// describe two different windows. Every human view resolves the window it
	// reports through Floor -- a blown weekly is what a user has to be told
	// about -- and then has nothing but Slack to reach for, which is a number
	// measured on some other window entirely. The live shape: a five-hour
	// window 85% elapsed at 98% used binds with 3.667 of slack while the weekly
	// beside it is 92% elapsed with nothing left in it, reporting 8.667 against
	// a pace target of 108.667. A bar drawn at the weekly's 100% and coloured
	// from the binding window's 3.667 is a full bar that says "plenty of room".
	//
	// Taken in the pass that already computes them rather than recovered by a
	// second one. A second pass would have to rebuild the window set, and then
	// there is a way for a window to be admitted to one and narrowed out of the
	// other -- which is the same failure the floor selection sits inside this
	// loop to avoid.
	FloorSlack     float64
	FloorThreshold float64
}

// modelFamilies are the family tokens ccdad can recognize inside a model name.
//
// It is deliberately a list of FAMILIES rather than of models. The wire gives
// per-model windows only a display name — "Opus 4.5", "Fable" — which carries a
// version ccdad has no business tracking, and a user typing --model types the
// family too. Matching on the family is the part of the name that both sides
// agree on and that a new release does not invalidate.
var modelFamilies = []string{"opus", "sonnet", "haiku", "fable"}

// ModelFamily reduces a model name to the family a per-model window is scoped
// to, and reports whether it could tell at all.
//
// It answers false rather than guessing, and both false cases matter. A name
// carrying no known family is a model ccdad has not heard of, and a name
// carrying two is a string that is not a model name at all. Callers read false
// as "cannot rule this window out", which keeps an unrecognized name on the
// conservative side: it never narrows anything away.
func ModelFamily(name string) (string, bool) {
	lower := strings.ToLower(name)
	found := ""
	for _, f := range modelFamilies {
		if !strings.Contains(lower, f) {
			continue
		}
		if found != "" {
			return "", false
		}
		found = f
	}
	if found == "" {
		return "", false
	}
	return found, true
}

// ModelFamilyNames lists the families ModelFamily knows, in its own order, so a
// CLI can build its refusal out of the same list the matcher reads.
func ModelFamilyNames() []string {
	out := make([]string, len(modelFamilies))
	copy(out, modelFamilies)
	return out
}

// FixedWindowFamily is the family a FIXED window is scoped to. It is written
// out rather than derived from the window's name, so that a future window
// whose key happens to contain a family token does not become model-scoped by
// accident.
//
// This is the one place the correspondence is spelled. internal/daemon's
// probeModel and internal/cli's probeWindow both go through it — the first
// forward, the second through WindowForFixedFamily's reverse table — rather
// than each carrying its own switch, which is how the same two cases used to
// drift out of sync silently: a family added here without a matching case
// there left a probe that woke nothing and cost a turn every six hours,
// forever, with no test able to see the other switch at all.
func FixedWindowFamily(n usage.WindowName) (string, bool) {
	switch n {
	case usage.WindowSevenDayOpus:
		return "opus", true
	case usage.WindowSevenDaySonnet:
		return "sonnet", true
	}
	return "", false
}

// fixedWindows is every window FixedWindowFamily answers for. Listed once so
// that WindowForFixedFamily's reverse lookup and
// TestFixedWindowFamilyAndItsReverseAgreeOverEveryFixedWindow both walk it,
// rather than either repeating FixedWindowFamily's own cases.
var fixedWindows = []usage.WindowName{usage.WindowSevenDayOpus, usage.WindowSevenDaySonnet}

// WindowForFixedFamily is FixedWindowFamily's reverse: the fixed window one
// family's turn has to be spent against to reach it. A family FixedWindowFamily
// does not answer for is refused here too — a --model naming a real family
// with no fixed window of its own still falls through to the caller's own
// five-hour fallback, which is the honest answer for a family this build
// cannot scope.
func WindowForFixedFamily(family string) (usage.WindowName, bool) {
	for _, n := range fixedWindows {
		if fam, _ := FixedWindowFamily(n); fam == family {
			return n, true
		}
	}
	return "", false
}

// bindingWindows is the set of windows that bind for one ranking pass: the
// --model narrowing rule (README, "ccdad switch").
//
// With no model named EVERY window binds, scoped ones included. That is the
// direction the fixed five already went — seven_day_opus binds today whatever a
// session is going to run — and it is the same asymmetry HeadroomOf was written
// on: counting a window that does not bind costs one conservative switch, while
// missing one that does bind hands work to an account that is already out of
// quota.
//
// Naming a model NARROWS, and only ever narrows: the per-model windows scoped to
// a DIFFERENT family drop out, and nothing new is added. So --model can only
// raise an account's headroom, never lower it, and a caller that names a model
// cannot end up more pessimistic than one that does not.
//
// Two kinds of window are never narrowed away. One whose family cannot be
// identified stays, because "I do not recognize this model" is not evidence that
// it is a different one. And a SURFACE-scoped window stays whatever the model
// is: Claude Code is itself one surface, so a surface weekly cap can be the very
// window that binds a ccdad session, and the wire gives no way to tell which
// surface name is this client's own. That last one is the deliberate seam here —
// it means a spent cap on some other surface reads as a spent account.
//
// One kind of window is added rather than narrowed, and only on the user's say
// so: a weekly cap filed under a scope key this build does not name. It is real
// quota and it is carried all the way here, but ccdad cannot state what it caps,
// so binding on it by default would have an account report a window whose
// meaning nobody can give. A threshold naming it in the per-window table IS the
// say so, and nothing else is: consent is per window, because a table set for
// five_hour is not a statement about a scope the user has never seen.
func bindingWindows(s *usage.Snapshot, model string, t Thresholds) []usage.NamedWindow {
	if s == nil {
		return nil
	}
	want, narrow := "", false
	if model != "" {
		want, narrow = ModelFamily(model)
	}
	fixed := s.RateLimitWindows()
	scoped := s.ScopedWindows()
	unknown := s.UnknownScopeWindows()
	out := make([]usage.NamedWindow, 0, len(fixed)+len(scoped)+len(unknown))
	for _, w := range fixed {
		if fam, ok := FixedWindowFamily(w.Name); ok && narrow && fam != want {
			continue
		}
		out = append(out, w)
	}
	for _, w := range scoped {
		if w.Model != "" && narrow {
			if fam, ok := ModelFamily(w.Model); ok && fam != want {
				continue
			}
		}
		out = append(out, w.NamedWindow)
	}
	for _, w := range unknown {
		// The map is read directly rather than through Thresholds.For, which
		// answers the default for a window with no entry and so cannot tell an
		// opted-in window from any other. The positivity test is the one For
		// applies: it treats a non-positive entry as the same omission, and a
		// gate that read consent where For reads omission would admit a window
		// and then measure it against a threshold nobody set for it.
		if v, opted := t.PerWindow[w.Name]; !opted || v <= 0 {
			continue
		}
		// No model narrowing: an unnamed scope has no model half to compare, and
		// dropping it because the family is unreadable would undo the opt-in the
		// user just gave.
		out = append(out, w.NamedWindow)
	}
	return out
}

// HeadroomOf finds the binding window for a session that has not named a model,
// which is every caller that is reporting rather than choosing.
func HeadroomOf(s *usage.Snapshot, t Thresholds) Headroom {
	return HeadroomFor(s, "", t)
}

// HeadroomFor finds the binding window for a session that will run model,
// against that pass's own per-window thresholds.
//
// The set it ranges over excludes cinder_cove: that is a one-time credit grant
// whose resets_at is an expiry, so a spent one would otherwise read as a
// permanently exhausted account. seven_day_oauth_apps IS included even though
// it is the one of the fixed five that reads as though it belonged to some
// other client — Claude Code is itself an OAuth app, nothing in the bundle says
// the window means anything else, and counting a window that does not bind is
// the cheaper of the two mistakes.
//
// Windows tie on the first in the schema's own order, and the scoped ones come
// after the fixed five in wire order, so the answer does not depend on map
// iteration.
func HeadroomFor(s *usage.Snapshot, model string, t Thresholds) Headroom {
	out := Headroom{}
	floorSlack, floorEmpty := 0.0, false
	for _, w := range bindingWindows(s, model, t) {
		pct, ok := w.Percent()
		if !ok {
			continue
		}
		thr := t.For(w.Name)
		slack := thr - pct
		// Taken BEFORE the binding test below sets Known, so the first readable
		// window seeds the minimum rather than being compared against a zero
		// that no window reported.
		if room := 100 - pct; !out.Known || room < out.MinPct {
			out.MinPct, out.MinWindow = room, w.Name
		}
		// The !out.Known guard is what makes the first readable window win
		// outright. out.Slack is zero before it, so a first window with any
		// positive slack would otherwise never be taken.
		if !out.Known || slack < out.Slack {
			out.Pct, out.Slack, out.Threshold = 100-pct, slack, thr
			out.Binding, out.Known = w.Name, true
		}
		// The weekly floor is picked up in the SAME pass rather than in a second
		// one over a second window set, so there is no way for a window to be
		// admitted to one and narrowed out of the other.
		//
		// Two ways to be a floor, and the second is purely ADDITIVE: every
		// window that qualified before still qualifies, so a user who set a
		// weekly threshold of their own sees exactly what they saw.
		//
		//   - PAST THE NUMBER IT WAS GIVEN. This is the configured-threshold
		//     reading and it is the right one there: a threshold a person typed
		//     IS the line past which they consider the account unusable, so a
		//     weekly at 85 under a threshold of 80 has to clear before the
		//     account is theirs to use again.
		//   - EMPTY. Under hover no threshold is a stop line -- it is a pace
		//     target -- so "past it" says nothing about whether the account is
		//     usable, and the test above answers on a figure that moves with the
		//     SIZE OF THE POOL. Worse, a pace target runs past 100 late in a
		//     window, so a weekly with nothing left in it reported POSITIVE
		//     slack and was not a floor at all. That is the case this arm is
		//     for, and it is the one that matters: a blown weekly holds the
		//     account back until it rolls, whatever any threshold says.
		empty := pct >= 100
		// The READING is asked, not the name. A codex window's length is a
		// property of the plan and only the endpoint knows it, so the same name
		// runs thirty days on one account and a week on another; a reading that
		// carried no length falls back to the name, which is every Claude
		// window and so every existing answer here.
		if (slack < 0 || empty) && usage.IsWeeklyOf(w.Name, w.Window) {
			// An EMPTY floor outranks one that is merely past its number,
			// whatever the slack says. Least-slack alone is the wrong key once
			// the two arms can both fire: a blown weekly reporting +8 of slack
			// against a pace target above 100 would lose to one five points past
			// its threshold and still holding quota, and it is the blown one
			// that decides when the account comes back.
			better := !out.HasFloor || (empty && !floorEmpty) ||
				(empty == floorEmpty && slack < floorSlack)
			if better {
				out.Floor, floorSlack, floorEmpty, out.HasFloor = w.Name, slack, empty, true
				// The reported pair is taken HERE, at the moment the window is
				// selected, so it can never describe a window other than the one
				// out.Floor names. Recovering it afterwards would mean a second
				// lookup keyed on a name, and a lookup can miss.
				out.FloorSlack, out.FloorThreshold = slack, thr
			}
		}
	}
	return out
}

// neverSpent reports whether a window has never been spent against: nothing
// used and no reset time to show for it.
//
// The utilization test is not redundant with the reset test. A window reporting
// a percentage above zero and NO reset is not an unused window — it is a
// resets_at this build could not read — and spending a turn on it would buy the
// same unreadable field back, once per rung of the ladder, forever.
func neverSpent(w usage.NamedWindow) bool {
	pct, ok := w.Percent()
	if !ok || pct != 0 {
		return false
	}
	_, has := w.Reset()
	return !has
}

// ColdWindow is the first window in an account's own candidate set — the same
// set HeadroomFor ranges over — whose clock is not running, together with the
// rollover the reading reported for it and whether there was one.
//
// A five-hour window is anchored at first use and does not stretch when more is
// spent against it, so a clock started early is elapsed time the account gets
// for free — that is what a warm-up buys, and this names the window that has
// none running.
//
// There are two ways to have no clock, and both are here because a warm-up
// answers both:
//
//   - NEVER SPENT: nothing used, no reset. This is the original question, and
//     the one an account added five minutes ago asks.
//   - ROLLED OVER: the reading carries a reset that has already PASSED. The
//     window ran down; the cached reading is simply older than the event. This
//     arm is why the warm-up can land on the same tick as the poll that finds
//     the window cold — without it the daemon must wait for one poll to write
//     {0, null} and then for the NEXT poll's tick to notice, and both intervals
//     are the ten-minute idle cadence.
//
// A window at more than 0% with no reset is in neither arm; neverSpent says why.
//
// It does not stop at the single BINDING window the way HeadroomFor does. An
// account's five-hour window can sit with no clock while a weekly cap binds
// tighter and has a running one of its own: from the ranking's point of view
// that account has pace already, but the five-hour clock is still stopped, and
// stopping is what costs lockout later. Ties resolve on the schema's own order,
// the same way HeadroomFor's do, which in practice means the plain five-hour
// window — first in wire order — wins over a weekly one when both qualify, and
// that is the window worth having.
func ColdWindow(s *usage.Snapshot, model string, t Thresholds, now time.Time) (usage.WindowName, time.Time, bool) {
	for _, w := range bindingWindows(s, model, t) {
		if neverSpent(w) {
			return w.Name, time.Time{}, true
		}
		if at, has := w.Reset(); has && !at.After(now) {
			return w.Name, at, true
		}
	}
	return "", time.Time{}, false
}

// WarmUpWouldSpendCredits reports whether spending one turn on this account
// could land on METERED CREDITS rather than on quota already paid for.
//
// It is the one gate the warm-up loop shares with the credit gate's reasoning,
// and it is here rather than in the daemon so that `ccdad hover status` refuses
// on the same predicate it refuses on. Fully automatic must not become fully
// automatic SPENDING: the ceiling and the account's own overage switch are the
// two independent opt-ins unattended overage requires, and a mode cannot supply
// either on the user's behalf — least of all a mode whose whole job is to spend
// small turns nobody asked for one at a time.
//
// Two conditions, and the first is the narrow one on purpose. A turn only
// reaches credits when there is no subscription quota LEFT, which is a window at
// 100%, not a window past the pace threshold hover derived for it: an account
// 80% through its week against a 46% pace target has a fifth of its quota in
// hand and the turn lands on that. Reading "past its threshold" as "out" here
// would have stopped warming five of the six accounts on the fleet this was
// written against, which is most of the benefit for none of the risk.
//
// The second fails closed the way credit.go's every branch does. Only an
// account whose overage is demonstrably off — Disabled, or Blocked by an org or
// seat policy — is evidence that a turn CANNOT be billed. Unknown is not, and an
// unread extra_usage is the state a bug looks like.
func WarmUpWouldSpendCredits(s *usage.Snapshot, model string, t Thresholds) bool {
	if s == nil || !capped(s, model, t) {
		return false
	}
	switch s.ExtraUsage.State {
	case usage.ExtraUsageDisabled, usage.ExtraUsageBlocked:
		return false
	}
	return true
}

// capped reports whether any window this account is measured on has nothing left
// in it.
//
// Every window rather than the binding one: HeadroomFor picks the least SLACK,
// which under a derived per-window threshold is not the most spent window, and
// the question here is about quota rather than about pace.
func capped(s *usage.Snapshot, model string, t Thresholds) bool {
	for _, w := range bindingWindows(s, model, t) {
		if pct, ok := w.Percent(); ok && pct >= 100 {
			return true
		}
	}
	return false
}

// NextResetAmong is the earliest FUTURE rollover among an account's candidate
// windows, or false when no window has one.
//
// FUTURE, and over the whole candidate set rather than the binding window: the
// caller is scheduling the poll that will find a clock stopped, and the binding
// window is routinely a weekly one whose reset is days out while the five-hour
// clock this exists to restart runs down within hours. A reset already in the
// past is not a schedule either — it is the event that has already happened, and
// ColdWindow is what answers for it.
func NextResetAmong(s *usage.Snapshot, model string, t Thresholds, now time.Time) (time.Time, bool) {
	out, found := time.Time{}, false
	for _, w := range bindingWindows(s, model, t) {
		at, has := w.Reset()
		if !has || !at.After(now) {
			continue
		}
		if !found || at.Before(out) {
			out, found = at, true
		}
	}
	return out, found
}

// recoveryOf is when the named window rolls over: the moment it stops holding
// the account back. A window that reported no reset has no recovery, which is
// not the same as recovering now.
//
// The name it is given is not always the binding one. A blown weekly window
// still holds the account back after the five-hour window has come back, so
// measure asks about the weekly floor when there is one.
//
// It searches ALL of the account's windows rather than the narrowed set the
// headroom came from. The name it is given was produced by that narrowed set, so
// the two agree; searching the wider one only means this function does not have
// to be told which model the pass was for.
//
// The search itself is Snapshot.ResetFor, which the probe needs for the same
// question — is there a reset time for this window at all — and which is
// nil-safe on its own, so the guard this function used to carry is inside it.
// What is left here is the ranking's own vocabulary: a timeValue rather than a
// comma-ok pair.
func recoveryOf(s *usage.Snapshot, clears usage.WindowName) timeValue {
	at, ok := s.ResetFor(clears)
	return timeValue{at: at, ok: ok}
}

// OnCreditAxis reports whether this headroom was measured against a credit
// balance rather than a plan window.
//
// A caller needs it because the two are not interchangeable downstream: the
// credit axis has no reset, no rollover and no entry in Snapshot.AllWindows, so
// anything that resolves the binding name to a window finds nothing and has to
// know whether that absence is "unreadable" or "there is no window here by
// construction". The comparison lives here so the literal extra_usage is
// spelled in one place.
func (h Headroom) OnCreditAxis() bool {
	return h.Known && h.Binding == creditWindow
}

// HeadroomOrCredit is the headroom to SHOW for an account: its binding plan
// window when it has one, and its credit allowance when it has none.
//
// It exists because HeadroomOf reads plan windows and nothing else, while
// rank.go:measure reassigns a primary credit seat onto the credit axis before
// ranking it. Every caller that rendered HeadroomOf therefore printed "?" for a
// seat the engine was ranking on a perfectly good number — the dashboard and
// the engine describing one account with two answers.
//
// The order is the same one Classify makes for the same reading, and it has to
// be: an account with a live window and overage switched on is metered by the
// window, and its credits are what it spends AFTER that window runs out. So a
// window that binds always wins, and the credit axis is reached only when there
// is no window at all.
//
// It is not a replacement for HeadroomOf. The RANKING must keep using the
// window-only form plus the primary-seat reassignment, because whether a credit
// balance may be spent at all is the credit gate's question and not this one;
// answering it here would rank a non-primary credit account as though its money
// were quota. This is the DISPLAY axis: what is left, on whichever meter this
// account actually runs on.
func HeadroomOrCredit(s *usage.Snapshot, t Thresholds) Headroom {
	if h := HeadroomOf(s, t); h.Known {
		return h
	}
	var e usage.ExtraUsage
	if s != nil {
		e = s.ExtraUsage
	}
	return creditHeadroomOf(e, t)
}
