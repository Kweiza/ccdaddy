package strategy

import (
	"strings"

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
	// Known is false when no window reported a utilization. An account that
	// could not be read is NOT an empty one, and treating it as one is the
	// exact bug that parked cswap's engine permanently.
	Known bool
	// Binding names the window Pct, Slack and Threshold all came from: the one
	// with the least slack. It is the ORDERING axis and the spent test, and the
	// three figures describe ONE window deliberately, so that Slack is always
	// Threshold minus that window's utilization.
	Binding usage.WindowName
	// Floor names the WEEKLY window the account is already past — the weekly
	// with the least slack among those below zero — and HasFloor is whether
	// there is one at all.
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

// fixedWindowFamily is the family a FIXED window is scoped to. It is written out
// rather than derived from the window's name, so that a future window whose key
// happens to contain a family token does not become model-scoped by accident.
func fixedWindowFamily(n usage.WindowName) (string, bool) {
	switch n {
	case usage.WindowSevenDayOpus:
		return "opus", true
	case usage.WindowSevenDaySonnet:
		return "sonnet", true
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
func bindingWindows(s *usage.Snapshot, model string) []usage.NamedWindow {
	if s == nil {
		return nil
	}
	want, narrow := "", false
	if model != "" {
		want, narrow = ModelFamily(model)
	}
	fixed := s.RateLimitWindows()
	scoped := s.ScopedWindows()
	out := make([]usage.NamedWindow, 0, len(fixed)+len(scoped))
	for _, w := range fixed {
		if fam, ok := fixedWindowFamily(w.Name); ok && narrow && fam != want {
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
	floorSlack := 0.0
	for _, w := range bindingWindows(s, model) {
		pct, ok := w.Percent()
		if !ok {
			continue
		}
		thr := t.For(w.Name)
		slack := thr - pct
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
		if slack < 0 && usage.IsWeekly(w.Name) && (!out.HasFloor || slack < floorSlack) {
			out.Floor, floorSlack, out.HasFloor = w.Name, slack, true
		}
	}
	return out
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
func recoveryOf(s *usage.Snapshot, clears usage.WindowName) (t timeValue) {
	if s == nil {
		return t
	}
	for _, w := range s.AllWindows() {
		if w.Name != clears {
			continue
		}
		if at, ok := w.Reset(); ok {
			return timeValue{at: at, ok: true}
		}
	}
	return t
}
