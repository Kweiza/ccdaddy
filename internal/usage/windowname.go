package usage

import (
	"errors"
	"fmt"
	"strings"
)

// A window name is a key a USER can type, not only a name the wire produced:
// `ccdad config` takes a threshold per window, so which names mean something
// has to be decidable without a reading in hand.
//
// It lives here rather than at the config layer because the prefix is this
// package's own. ScopedWindows builds a synthetic name out of it and this file
// recognizes one by it, so a name usage can produce is a name usage accepts.
// Spelled twice, the two would drift the first time either changed, and the
// symptom would be a threshold a user set that quietly never applied.

// The two scopes Claude Code models a weekly_scoped entry under. Its cached-usage
// schema types `scope` as {model?: {display_name}, surface?: {display_name}} and
// names no third key, which is where these two come from. That schema is a zod
// passthrough at every level and `kind` is a plain string rather than an enum, so
// it is Claude Code's read model and not a closed server contract: a third scope
// key would survive its parse, and ScopedWindows here would drop the entry for
// want of a name it recognizes.
//
// Claude Code's own projection is NARROWER than ScopedWindows rather than the
// same rule. It filters `n.kind === "weekly_scoped" && n.scope?.model`, then
// intersects the model display name with the tengu_usage_overage_included_models
// allowlist, and never reads the surface at all — an entry scoped only to a
// surface is dropped there and kept here. That projection feeds a usage dialog;
// which caps to DRAW is a different question from which caps a session is
// subject to, so following it exactly would lose windows that can bind.
//
// They are exported so no caller outside this package has to spell either again.
const (
	ScopeModel   = "model"
	ScopeSurface = "surface"
)

// ScopedWindowName is the name a scoped weekly cap is filed under. scope is
// ScopeModel or ScopeSurface, and display is the scope's DISPLAY name,
// verbatim, because that is the only handle the wire gives it — there is no
// stable identifier in the scope object.
//
// The scope is in the name and not only the display half, so a model and a
// surface that share a display name stay two windows.
func ScopedWindowName(scope, display string) WindowName {
	return WindowName(scopedPrefix(scope) + display)
}

// scopedPrefix is everything in a scoped name before the display half. Both the
// constructor and the validator go through it, which is what makes the two
// halves impossible to change apart.
func scopedPrefix(scope string) string {
	return weeklyScopedKind + ":" + scope + ":"
}

// scopedWindowScopes is the scope set, in the order ScopedWindows resolves it: a
// model display name wins when an entry carries both, which is the half Claude
// Code's own filter requires.
//
// The MEMBERSHIP lives in one table because both halves walk it — ScopedWindows
// to name an entry, ValidWindowName to recognize a name. Listed twice, a scope
// added to one and not the other is silent either way round: a window nobody can
// set a threshold on, or a threshold on a window no reading can produce.
var scopedWindowScopes = [...]struct {
	Name    string
	Display func(Limit) string
}{
	{ScopeModel, func(l Limit) string { return l.ModelDisplayName }},
	{ScopeSurface, func(l Limit) string { return l.SurfaceDisplayName }},
}

// rateLimitWindowNames is Snapshot.RateLimitWindows' membership, in the same
// order. cinder_cove is out for the reason it is out there: its resets_at is an
// expiry rather than a rollover, so nothing ever ranks it.
var rateLimitWindowNames = [...]WindowName{
	WindowFiveHour,
	WindowSevenDay,
	WindowSevenDayOAuthApps,
	WindowSevenDayOpus,
	WindowSevenDaySonnet,
}

// RateLimitWindowNames is the five recurring windows by name, for a caller that
// has no reading to take them from — help text, a refusal, a settable-key list.
// It returns a copy so a caller cannot edit the table every answer comes from.
func RateLimitWindowNames() []WindowName {
	out := make([]WindowName, len(rateLimitWindowNames))
	copy(out, rateLimitWindowNames[:])
	return out
}

// ErrUnknownScope marks the one refusal that is an OPT-IN rather than a
// rejection: a well-formed scoped name filed under a scope key this build does
// not name.
//
// It is a refusal because no reading this build takes produces that window, so
// ValidWindowName cannot answer nil and let a caller believe the name is live.
// It is distinguishable because a caller that OFFERS the opt-in still has to
// refuse an outright misspelling, and the two arrive here looking identical:
// nothing without a reading in hand can tell weekly_scoped:region:eu from
// weekly_scoped:modle:Fable. A caller that offers the opt-in says so; a caller
// that does not treats this as any other refusal.
//
// The opt-in itself is a window_threshold entry naming the window. The config
// loader carries such an entry whatever its name is, so the opt-in works from a
// hand-written file today; this error is the shape a prompt needs to offer it.
var ErrUnknownScope = errors.New("the window is scoped to a key this build does not name")

// ValidWindowName reports why n is not a window a threshold may be set on, and
// nil when it is one.
//
// It returns an error rather than a bool because every caller has a user to
// tell, and the refusals are four different sentences. Only one of them is "that
// is not a window name". cinder_cove's name is perfectly real and only its
// rollover is not, so saying that would be a lie. A scoped name with no display
// half has the right SHAPE and nothing to attribute. And a scoped name under a
// key this build does not know is the one that wraps ErrUnknownScope, because
// the answer a caller wants there is "not yet" rather than "no".
//
// It is deliberately narrower than WindowName.Scoped(), which tests only the
// weekly_scoped: prefix and therefore answers true for weekly_scoped: and
// weekly_scoped:region:. Neither is a name any reading can produce.
func ValidWindowName(n WindowName) error {
	for _, known := range rateLimitWindowNames {
		if n == known {
			return nil
		}
	}
	// cinder_cove gets its own sentence: the user needs to know the refusal is
	// about what the window IS rather than about how they spelled it.
	if n == WindowCinderCove {
		return fmt.Errorf("%q is a one-time credit grant whose resets_at is an expiry rather than a rollover, so it is never ranked and a threshold on it would never be read", n)
	}
	// The scoped shape is parsed once and generically rather than tried against
	// each known prefix in turn, because a name under an UNKNOWN scope has to
	// reach a sentence of its own — and a loop over the known scopes can only
	// ever fall through to the sentence for a name that has no shape at all.
	rest, scoped := strings.CutPrefix(string(n), weeklyScopedKind+":")
	if !scoped {
		return unknownWindowName(n)
	}
	scope, display, split := strings.Cut(rest, ":")
	if !split || scope == "" {
		return unknownWindowName(n)
	}
	if display == "" {
		// ScopedWindows drops an entry it cannot attribute, so a name with
		// nothing after the scope names a window that can never exist.
		return fmt.Errorf("%q names no display name: %s must be followed by the display name the cap is scoped to", n, scopedPrefix(scope))
	}
	for _, sc := range scopedWindowScopes {
		if scope == sc.Name {
			return nil
		}
	}
	return fmt.Errorf("%q is filed under the scope %q, which this build names no window under: it ranks only once a reading actually carries that scope, and until one does a threshold on it applies to nothing: %w", n, scope, ErrUnknownScope)
}

// unknownWindowName is the sentence for a name with no shape this package can
// place. It lists what does exist, because rejecting a name without offering one
// leaves the user nowhere to go.
func unknownWindowName(n WindowName) error {
	return fmt.Errorf("%q is not a window name: it must be one of %s, or a scoped weekly name beginning %s",
		n, fixedWindowNameList(), scopedPrefixList())
}

// fixedWindowNameList renders the five for a refusal, from the same table the
// refusal checks against.
func fixedWindowNameList() string {
	out := make([]string, 0, len(rateLimitWindowNames))
	for _, n := range rateLimitWindowNames {
		out = append(out, string(n))
	}
	return strings.Join(out, ", ")
}

// scopedPrefixList renders the scoped prefixes for a refusal, from the same
// table the refusal checks against. A scope this build knows and does not offer
// is a name the user is never told about.
func scopedPrefixList() string {
	out := make([]string, 0, len(scopedWindowScopes))
	for _, sc := range scopedWindowScopes {
		out = append(out, scopedPrefix(sc.Name))
	}
	return strings.Join(out, " or ")
}
