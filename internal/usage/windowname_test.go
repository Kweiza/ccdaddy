package usage

import (
	"strconv"
	"strings"
	"testing"
)

// Every window the ranking can bind on must be a name a threshold can be
// attached to. The two lists are built separately, so this is the gate that
// keeps them the same list.
func TestEveryRankedWindowIsAValidThresholdTarget(t *testing.T) {
	names := RateLimitWindowNames()
	ranked := (&Snapshot{}).RateLimitWindows()
	if len(names) != len(ranked) {
		t.Fatalf("RateLimitWindowNames() has %d names and RateLimitWindows() has %d windows; a window that can bind but cannot carry a threshold is a window nobody can tune",
			len(names), len(ranked))
	}
	for i, w := range ranked {
		if names[i] != w.Name {
			t.Fatalf("RateLimitWindowNames()[%d] = %q, want %q — the two lists must stay in the schema's own order", i, names[i], w.Name)
		}
	}
	for _, n := range names {
		if err := ValidWindowName(n); err != nil {
			t.Errorf("ValidWindowName(%q) = %v; a window the engine ranks must be one a user can set a threshold on", n, err)
		}
	}
}

// cinder_cove is a one-time grant whose resets_at is an expiry, which is why
// RateLimitWindows leaves it out. Accepting a threshold on it would write a
// setting nothing ever reads, and the refusal has to say that rather than
// "unknown name" — the name is perfectly real.
func TestCinderCoveIsRefusedAndTheRefusalSaysWhy(t *testing.T) {
	err := ValidWindowName(WindowCinderCove)
	if err == nil {
		t.Fatal("ValidWindowName(cinder_cove) = nil; a threshold on it is a setting with no effect")
	}
	if !strings.Contains(err.Error(), string(WindowCinderCove)) {
		t.Errorf("error = %q, want it to name the window", err)
	}
	if !strings.Contains(err.Error(), "expiry") {
		t.Errorf("error = %q, want it to say why cinder_cove is not ranked", err)
	}
}

// Scoped() tests only the weekly_scoped: prefix, so it answers true for a scope
// no reading can produce. The validator is the narrower question.
func TestAScopedNameIsAcceptedOnlyUnderAScopeThisPackageProduces(t *testing.T) {
	for _, ok := range []WindowName{
		ScopedWindowName(ScopeModel, "Opus 4.5"),
		ScopedWindowName(ScopeSurface, "Cowork"),
		"weekly_scoped:model:Fable",
		"weekly_scoped:surface:Claude Code",
	} {
		if err := ValidWindowName(ok); err != nil {
			t.Errorf("ValidWindowName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []WindowName{
		"weekly_scoped:region:eu",
		"weekly_scoped:anything:x",
		"weekly_scoped:",
		ScopedWindowName(ScopeModel, ""),
		ScopedWindowName(ScopeSurface, ""),
	} {
		// Scoped() answers true for every one of these, which is exactly why it
		// is not the validator.
		if !bad.Scoped() {
			t.Fatalf("Scoped(%q) = false; this case no longer demonstrates the difference the validator exists for", bad)
		}
		if err := ValidWindowName(bad); err == nil {
			t.Errorf("ValidWindowName(%q) = nil; ScopedWindows can never produce that name, so a threshold under it would never be read", bad)
		}
	}
}

// The refusal is the only guide a user gets back to a name that works, so it
// has to list what does exist rather than only reject what does not — and it has
// to list ALL of them, because the one a user is told about is the one they can
// reach.
func TestTheRefusalListsTheNamesThatDoExist(t *testing.T) {
	const typo = "five_hourr"
	err := ValidWindowName(typo)
	if err == nil {
		t.Fatal("ValidWindowName(five_hourr) = nil; an accepted typo is a threshold that silently does nothing")
	}
	// The message quotes the name it refused, and a near-miss typo CONTAINS the
	// name it was a near miss for. Searching the whole message would find
	// five_hour inside the user's own five_hourr and pass with the list empty, so
	// the quoted input comes out before anything is looked for.
	guidance := strings.ReplaceAll(err.Error(), strconv.Quote(typo), "")

	want := make([]string, 0, len(rateLimitWindowNames)+len(scopedWindowScopes))
	for _, n := range RateLimitWindowNames() {
		want = append(want, string(n))
	}
	for _, sc := range scopedWindowScopes {
		want = append(want, scopedPrefix(sc.Name))
	}
	for _, w := range want {
		if !strings.Contains(guidance, w) {
			t.Errorf("error = %q, want it to offer %q — a name the refusal does not list is a window the user cannot find", err, w)
		}
	}
}

// The prefix has to be at the FRONT of the name. A containment test would make
// "junk weekly_scoped:model:Fable" valid, and a threshold under it would never be
// read because no reading produces that name. The empty name is the same defect
// from the other end: `ccdad config set window_threshold. 85` reaches here with
// nothing after the dot, and accepting it writes a key nothing ever looks up.
func TestANameIsRefusedUnlessTheWholeOfItIsTheName(t *testing.T) {
	for _, bad := range []WindowName{
		"",
		" ",
		"junk weekly_scoped:model:Fable",
		"xweekly_scoped:model:Fable",
		"five_hour weekly_scoped:surface:Cowork",
		" five_hour",
		"five_hour ",
		"FIVE_HOUR",
	} {
		if err := ValidWindowName(bad); err == nil {
			t.Errorf("ValidWindowName(%q) = nil; ScopedWindows cannot produce that name and the six keys are not spelled that way, so a threshold under it would never be read", bad)
		}
	}
}

// A bare prefix is the refusal whose SHAPE is right and whose display half is
// missing, which is why it does not share the unknown-name sentence: sending a
// user to look for a spelling mistake in a name that is spelled correctly is the
// one answer that cannot lead anywhere.
func TestABarePrefixIsRefusedForItsMissingDisplayNameRatherThanForItsSpelling(t *testing.T) {
	for _, sc := range scopedWindowScopes {
		n := ScopedWindowName(sc.Name, "")
		err := ValidWindowName(n)
		if err == nil {
			t.Fatalf("ValidWindowName(%q) = nil; ScopedWindows drops an entry that names no scope, so the name can never exist", n)
		}
		if !strings.Contains(err.Error(), "display name") {
			t.Errorf("error = %q, want it to say what is missing rather than only that the name was refused", err)
		}
		if strings.Contains(err.Error(), fixedWindowNameList()) {
			t.Errorf("error = %q, want its own sentence — offering the six fixed names answers a question the user did not ask", err)
		}
	}
}

// The constructor and the validator share one prefix builder, so a name this
// package builds is a name this package accepts. Without this, a change to one
// half would be invisible until a user's config stopped taking effect.
func TestEveryNameScopedWindowsBuildsPassesTheValidator(t *testing.T) {
	s := &Snapshot{Limits: []Limit{
		LimitFor(LimitInput{Kind: "weekly_scoped", Group: "model", Model: "Opus 4.5"}),
		LimitFor(LimitInput{Kind: "weekly_scoped", Group: "surface", Surface: "Cowork"}),
	}}
	ws := s.ScopedWindows()
	if len(ws) != 2 {
		t.Fatalf("ScopedWindows() = %v, want both entries", scopedNames(ws))
	}
	want := []WindowName{"weekly_scoped:model:Opus 4.5", "weekly_scoped:surface:Cowork"}
	for i, w := range ws {
		if w.Name != want[i] {
			t.Errorf("ScopedWindows()[%d].Name = %q, want %q — the name is on the wire's behalf and a caller looks a binding window back up by it", i, w.Name, want[i])
		}
		if err := ValidWindowName(w.Name); err != nil {
			t.Errorf("ValidWindowName(%q) = %v", w.Name, err)
		}
	}
}

// The table is package state that every refusal and every settable-key list is
// built from, so handing a caller the array itself lets one caller's sort or
// one caller's overwrite change what every later caller is told — and, because
// the validator answers from the same table, change which names are accepted.
func TestRateLimitWindowNamesHandsBackACopy(t *testing.T) {
	first := RateLimitWindowNames()
	first[0] = "five_hourr"

	if second := RateLimitWindowNames(); second[0] != WindowFiveHour {
		t.Errorf("RateLimitWindowNames()[0] = %q after a caller overwrote its own result, want %q — the two calls are sharing one array", second[0], WindowFiveHour)
	}
	if err := ValidWindowName(WindowFiveHour); err != nil {
		t.Errorf("ValidWindowName(%q) = %v; a caller's edit reached the table the validator answers from", WindowFiveHour, err)
	}
	if err := ValidWindowName("five_hourr"); err == nil {
		t.Error(`ValidWindowName("five_hourr") = nil; a caller's edit added a name to the table the validator answers from`)
	}
}
