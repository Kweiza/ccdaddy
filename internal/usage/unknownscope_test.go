package usage

import (
	"encoding/json"
	"errors"
	"testing"
)

// Claude Code types the scope object as {model?, surface?} and names no third
// key, but every level of that schema is a passthrough and `kind` is a plain
// string, so a scope key added server-side survives ITS parse. It has to survive
// this one too: a weekly cap the session is subject to that this build simply
// discards is quota the ranking spends without knowing it exists.
func TestAScopeThisBuildDoesNotNameSurvivesTheDecode(t *testing.T) {
	s := mustParse(t, `{"limits": [{"kind": "weekly_scoped", "group": "region", "percent": 40,
	  "resets_at": null, "scope": {"region": {"display_name": "eu"}}}]}`)

	ws := s.UnknownScopeWindows()
	if len(ws) != 1 {
		t.Fatalf("UnknownScopeWindows() = %v, want the region entry", scopedNames(ws))
	}
	if ws[0].Name != "weekly_scoped:region:eu" {
		t.Errorf("UnknownScopeWindows()[0].Name = %q, want weekly_scoped:region:eu — it is named by the key the wire filed it under", ws[0].Name)
	}
	if ws[0].Scope != "region" {
		t.Errorf("UnknownScopeWindows()[0].Scope = %q, want region", ws[0].Scope)
	}
	if pct, ok := ws[0].Percent(); !ok || pct != 40 {
		t.Errorf("Percent() = %v, %v; want 40, true — the reading is carried, not only the name", pct, ok)
	}
}

// It is carried, and it does not rank. ScopedWindows is the set the engine binds
// on, and a cap this build cannot attribute is not one it can tell a user about
// — the same reason an entry naming NO scope has always been dropped.
func TestAnUnknownScopeIsNotInTheSetTheEngineBindsOn(t *testing.T) {
	s := mustParse(t, `{"limits": [{"kind": "weekly_scoped", "group": "region", "percent": 40,
	  "resets_at": null, "scope": {"region": {"display_name": "eu"}}}]}`)

	if ws := s.ScopedWindows(); len(ws) != 0 {
		t.Errorf("ScopedWindows() = %v, want none — an unattributable cap must not rank by default", scopedNames(ws))
	}
}

// AllWindows is what a caller holding a binding window's NAME looks it up in, and
// status renders that name. If the opt-in puts an unknown-scope window into the
// ranking, the lookup has to find it or the account reports a binding window
// that does not exist.
func TestAnUnknownScopeIsStillLookedUpByName(t *testing.T) {
	s := mustParse(t, `{"limits": [{"kind": "weekly_scoped", "group": "region", "percent": 40,
	  "resets_at": null, "scope": {"region": {"display_name": "eu"}}}]}`)

	var found bool
	for _, w := range s.AllWindows() {
		if w.Name == "weekly_scoped:region:eu" {
			found = true
		}
	}
	if !found {
		t.Error("AllWindows() does not carry weekly_scoped:region:eu; a window the ranking can bind on that the name lookup cannot find reports a binding window nobody can render")
	}
}

// The cache round-trips a Snapshot through this package's own JSON. Dropping the
// scope on the way out would make the second read of a cached entry disagree
// with the first.
func TestAnUnknownScopeRoundTripsThroughTheCache(t *testing.T) {
	s := mustParse(t, `{"limits": [{"kind": "weekly_scoped", "group": "region", "percent": 40,
	  "resets_at": null, "scope": {"region": {"display_name": "eu"}}}]}`)

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	var back Snapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal(%s) = %v", b, err)
	}
	ws := back.UnknownScopeWindows()
	if len(ws) != 1 || ws[0].Name != "weekly_scoped:region:eu" {
		t.Errorf("after a round trip UnknownScopeWindows() = %v, want weekly_scoped:region:eu — encoded as %s", scopedNames(ws), b)
	}
}

// An entry can carry a scope this build names AND one it does not. It is one
// window either way, and the half that can be attributed is the half to name it
// by — otherwise the same cap would be counted twice, once in each set.
func TestAnEntryCarryingBothIsNamedByTheScopeThisBuildKnows(t *testing.T) {
	s := mustParse(t, `{"limits": [{"kind": "weekly_scoped", "group": "model", "percent": 40,
	  "resets_at": null, "scope": {"model": {"display_name": "Fable"}, "region": {"display_name": "eu"}}}]}`)

	ws := s.ScopedWindows()
	if len(ws) != 1 || ws[0].Name != "weekly_scoped:model:Fable" {
		t.Fatalf("ScopedWindows() = %v, want weekly_scoped:model:Fable", scopedNames(ws))
	}
	if un := s.UnknownScopeWindows(); len(un) != 0 {
		t.Errorf("UnknownScopeWindows() = %v, want none — the entry is already named, and counting it twice would rank one cap as two", scopedNames(un))
	}
}

// One limits[] entry is one cap, so it is one window however many scopes it
// carries — ranking it once per scope would count the same quota twice and let a
// user tighten a threshold that never binds because its twin binds first.
//
// Which scope names it cannot come from map order: Go randomises that, so the
// name would move between calls, and the ranking ties on the FIRST window in
// order. Sorted order is the choice, and it has to be the same choice every time
// far more than it has to be any particular one.
func TestAnEntryUnderTwoUnnamedScopesIsOneWindowNamedTheSameWayEveryTime(t *testing.T) {
	s := mustParse(t, `{"limits": [{"kind": "weekly_scoped", "group": "x", "percent": 10,
	  "resets_at": null, "scope": {"zone": {"display_name": "z"}, "region": {"display_name": "eu"}}}]}`)

	for i := 0; i < 50; i++ {
		ws := s.UnknownScopeWindows()
		if len(ws) != 1 {
			t.Fatalf("UnknownScopeWindows() = %v, want one window for one entry — one cap ranked twice is quota counted twice", scopedNames(ws))
		}
		if ws[0].Name != "weekly_scoped:region:eu" {
			t.Fatalf("UnknownScopeWindows() = %v, want weekly_scoped:region:eu on every call", scopedNames(ws))
		}
	}
}

// Entries keep WIRE order, the way ScopedWindows keeps it, because that is the
// order the ranking ties on and the order the endpoint itself chose.
func TestUnknownScopeWindowsKeepWireOrder(t *testing.T) {
	s := mustParse(t, `{"limits": [
	  {"kind": "weekly_scoped", "group": "zone",   "percent": 10, "resets_at": null, "scope": {"zone": {"display_name": "z"}}},
	  {"kind": "weekly_scoped", "group": "region", "percent": 20, "resets_at": null, "scope": {"region": {"display_name": "eu"}}}]}`)

	ws := s.UnknownScopeWindows()
	if len(ws) != 2 {
		t.Fatalf("UnknownScopeWindows() = %v, want both entries", scopedNames(ws))
	}
	if ws[0].Name != "weekly_scoped:zone:z" || ws[1].Name != "weekly_scoped:region:eu" {
		t.Errorf("UnknownScopeWindows() = %v, want zone then region — the wire's own order, not the key's", scopedNames(ws))
	}
}

// The schema is a passthrough, which means an unknown key may hold a shape this
// build has never seen. Failing the whole document over one would throw away the
// five windows that parsed perfectly.
func TestAScopeOfAShapeThisBuildDoesNotExpectDoesNotFailTheDocument(t *testing.T) {
	s := mustParse(t, `{"five_hour": {"utilization": 12, "resets_at": null},
	  "limits": [{"kind": "weekly_scoped", "group": "region", "percent": 40, "resets_at": null,
	  "scope": {"region": {"id": "eu-west-1"}, "model": {"display_name": "Fable"}}}]}`)

	if pct, ok := s.FiveHour.Percent(); !ok || pct != 12 {
		t.Errorf("FiveHour = %v, %v; want 12, true — one unreadable scope must not cost the windows that did read", pct, ok)
	}
	ws := s.ScopedWindows()
	if len(ws) != 1 || ws[0].Name != "weekly_scoped:model:Fable" {
		t.Errorf("ScopedWindows() = %v, want the model half to survive its neighbour", scopedNames(ws))
	}
}

// A key this build NAMES is a different question from one it does not. The wire
// says a model cap exists and then hands back a shape that cannot be read: that
// is an unreadable reading, not an absent one, and reading it as absent is the
// direction that makes a spent account look fresh. The document fails, exactly
// as it did before this file learned to tolerate other keys.
func TestAnUnreadableModelScopeStillFailsTheWholeDocument(t *testing.T) {
	for _, body := range []string{
		`{"limits": [{"kind": "weekly_scoped", "group": "model", "percent": 99, "resets_at": null,
		  "scope": {"model": {"display_name": 42}}}]}`,
		`{"limits": [{"kind": "weekly_scoped", "group": "surface", "percent": 99, "resets_at": null,
		  "scope": {"surface": {"display_name": ["Cowork"]}}}]}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("Parse(%s) = nil error; a model cap that refuses to say what it caps must not read as an account with no cap", body)
		}
	}
}

// The refusal has to be distinguishable, not merely present. A caller offering
// the opt-in for a scope this build does not name must still refuse an outright
// misspelling, and one error value is what it tells them apart by.
func TestAnUnnamedScopeIsRefusedDistinguishablyFromAMisspelling(t *testing.T) {
	err := ValidWindowName("weekly_scoped:region:eu")
	if err == nil {
		t.Fatal("ValidWindowName(weekly_scoped:region:eu) = nil; this build cannot produce that name, so accepting it silently would be a threshold nothing reads")
	}
	if !errors.Is(err, ErrUnknownScope) {
		t.Errorf("ValidWindowName(weekly_scoped:region:eu) = %v, want it to wrap ErrUnknownScope so a caller can offer the opt-in", err)
	}
	for _, plain := range []WindowName{"five_hourr", "cinder_cove", "weekly_scoped:", "weekly_scoped:model:"} {
		err := ValidWindowName(plain)
		if err == nil {
			t.Fatalf("ValidWindowName(%q) = nil", plain)
		}
		if errors.Is(err, ErrUnknownScope) {
			t.Errorf("ValidWindowName(%q) wraps ErrUnknownScope; a caller would offer to rank a window that cannot exist", plain)
		}
	}
}

// {"display_name": ...} is the shape both named scopes use and the one a new key
// is likeliest to follow, but it is not the only usable handle: a bare string is
// a display name with the wrapper left off. Reading only the object shape would
// leave the single most plausible third-key spelling reproducing the whole bug
// this file exists to close.
func TestABareStringScopeIsAUsableHandle(t *testing.T) {
	s := mustParse(t, `{"limits": [{"kind": "weekly_scoped", "group": "organization", "percent": 40,
	  "resets_at": null, "scope": {"organization": "acme"}}]}`)

	ws := s.UnknownScopeWindows()
	if len(ws) != 1 || ws[0].Name != "weekly_scoped:organization:acme" {
		t.Fatalf("UnknownScopeWindows() = %v, want weekly_scoped:organization:acme", scopedNames(ws))
	}
}

// A scope offering no handle names NOTHING, and the name is the whole point —
// ScopedWindowName with an empty display half builds a bare prefix, a string
// ValidWindowName refuses and no threshold can ever be set on. The entry is left
// out rather than carried as a window nobody can address.
//
// Each fixture holds ONLY the unreadable scope on purpose. With a readable one
// beside it the entry would be attributed and left out of this set anyway, so
// the assertion would pass whether the guard existed or not.
func TestAScopeOfferingNoHandleNamesNothing(t *testing.T) {
	for _, scope := range []string{
		`{"region": {}}`,
		`{"region": {"id": "eu-west-1"}}`,
		`{"region": {"display_name": ""}}`,
		`{"region": {"display_name": null}}`,
		`{"region": 5}`,
		`{"region": null}`,
		`{"region": []}`,
	} {
		body := `{"five_hour": {"utilization": 12, "resets_at": null},
		  "limits": [{"kind": "weekly_scoped", "group": "region", "percent": 40, "resets_at": null,
		  "scope": ` + scope + `}]}`
		s := mustParse(t, body)
		if un := s.UnknownScopeWindows(); len(un) != 0 {
			t.Errorf("with scope %s, UnknownScopeWindows() = %v, want none — a bare prefix is a name no threshold can be set on", scope, scopedNames(un))
		}
		if pct, ok := s.FiveHour.Percent(); !ok || pct != 12 {
			t.Errorf("with scope %s, FiveHour = %v, %v; want 12, true — one unreadable scope must not cost the windows that did read", scope, pct, ok)
		}
	}
}

// A window has to be findable by its own name, and the name is split on its
// FIRST colon. A scope key carrying one of its own builds a string that means
// something else on the way back: {"model:x": {"display_name": "y"}} and a model
// display-named "x:y" are the identical name, so one threshold would silently
// govern two caps. An empty key splits to an empty scope, which is not a name
// either. Neither can be addressed, so neither becomes a window.
func TestAScopeKeyThatCannotBeReadBackOutOfTheNameNamesNothing(t *testing.T) {
	for _, scope := range []string{
		`{"model:x": {"display_name": "y"}}`,
		`{"": {"display_name": "eu"}}`,
		`{"a:b:c": "d"}`,
	} {
		body := `{"limits": [{"kind": "weekly_scoped", "group": "x", "percent": 40, "resets_at": null,
		  "scope": ` + scope + `}]}`
		s := mustParse(t, body)
		if un := s.UnknownScopeWindows(); len(un) != 0 {
			t.Errorf("with scope %s, UnknownScopeWindows() = %v, want none — the name would not read back as the window it names", scope, scopedNames(un))
		}
		for _, w := range s.AllWindows() {
			if err := ValidWindowName(w.Name); err != nil && !errors.Is(err, ErrUnknownScope) {
				t.Errorf("with scope %s, AllWindows() carries %q, which ValidWindowName calls a non-name: %v", scope, w.Name, err)
			}
		}
	}
}

// The receiver is nil wherever a reading was never taken, and every other
// accessor on Snapshot answers that without panicking.
func TestUnknownScopeWindowsOnNoSnapshotIsEmpty(t *testing.T) {
	var s *Snapshot
	if ws := s.UnknownScopeWindows(); ws != nil {
		t.Errorf("UnknownScopeWindows() on a nil snapshot = %v, want nil", scopedNames(ws))
	}
}

// AllWindows lists the fixed five, then the attributed scoped windows, then the
// unnamed-scope ones. The order is not cosmetic: the ranking ties on the first
// window in order, and a caller resolving a name back to a window takes the
// first match.
func TestAllWindowsListsTheAttributedScopesBeforeTheUnnamedOnes(t *testing.T) {
	s := mustParse(t, `{"limits": [
	  {"kind": "weekly_scoped", "group": "region", "percent": 10, "resets_at": null, "scope": {"region": {"display_name": "eu"}}},
	  {"kind": "weekly_scoped", "group": "model",  "percent": 20, "resets_at": null, "scope": {"model": {"display_name": "Fable"}}}]}`)

	var got []WindowName
	for _, w := range s.AllWindows() {
		if w.Name.Scoped() {
			got = append(got, w.Name)
		}
	}
	want := []WindowName{"weekly_scoped:model:Fable", "weekly_scoped:region:eu"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("AllWindows() scoped half = %v, want %v — the attributed window comes first even though the wire listed it second", got, want)
	}
}

// LimitFor takes a map from a caller who keeps their own reference to it. A
// Limit that aliased it would change under the ranking whenever that caller
// reused their map, which is the same hazard RateLimitWindowNames returns a copy
// for.
func TestLimitForDoesNotAliasTheCallersScopeMap(t *testing.T) {
	m := map[string]string{"region": "eu"}
	l := LimitFor(LimitInput{Kind: "weekly_scoped", OtherScopes: m})
	m["region"] = "us"

	if got := l.OtherScopes["region"]; got != "eu" {
		t.Errorf("Limit.OtherScopes[region] = %q after the caller reused their map, want eu", got)
	}
}

// weekly_scoped is the one kind Claude Code reads as a rate-limit window. An
// entry of any other kind may be a one-time grant whose resets_at is an EXPIRY,
// and ranking one parks the engine waiting for a rollover that never comes —
// which is the cinder_cove rule, and it does not stop applying because the entry
// happens to carry a scope this build cannot name.
func TestUnknownScopeWindowsIgnoresAnUnknownKind(t *testing.T) {
	s := mustParse(t, `{"limits": [{"kind": "one_time_grant", "group": "region", "percent": 40,
	  "resets_at": null, "scope": {"region": {"display_name": "eu"}}}]}`)

	if un := s.UnknownScopeWindows(); len(un) != 0 {
		t.Errorf("UnknownScopeWindows() = %v, want none — an entry of an unknown kind may be a grant whose resets_at is an expiry", scopedNames(un))
	}
}

// The decode filters unusable scope keys out, so these guards are only reachable
// through LimitFor — which is how every test fixture and every caller outside
// this package builds a Limit. A guard nothing can reach from the wire is still
// a guard the constructor can walk into.
func TestAnEntryBuiltWithAnUnusableScopeKeyNamesNothingAndIsCounted(t *testing.T) {
	for name, scopes := range map[string]map[string]string{
		"no display half":  {"region": ""},
		"colon in the key": {"model:x": "y"},
		"empty key":        {"": "eu"},
	} {
		s := &Snapshot{Limits: []Limit{LimitFor(LimitInput{
			Kind: "weekly_scoped", OtherScopes: scopes,
		})}}
		if ws := s.UnknownScopeWindows(); len(ws) != 0 {
			t.Errorf("%s: UnknownScopeWindows() = %v, want none", name, scopedNames(ws))
		}
		if n := s.UnnamableLimits(); n != 1 {
			t.Errorf("%s: UnnamableLimits() = %d, want 1 — a cap dropped without a count is a cap dropped silently", name, n)
		}
	}
}

// A usable key is taken even when an unusable one sorts ahead of it. Indexing
// the sorted list at zero would name the window after the key that offers
// nothing and throw away the one that offers a handle.
func TestAUsableScopeKeyIsTakenOverAnUnusableOneThatSortsFirst(t *testing.T) {
	s := &Snapshot{Limits: []Limit{LimitFor(LimitInput{
		Kind:        "weekly_scoped",
		OtherScopes: map[string]string{"aaa": "", "region": "eu"},
	})}}

	ws := s.UnknownScopeWindows()
	if len(ws) != 1 || ws[0].Name != "weekly_scoped:region:eu" {
		t.Errorf("UnknownScopeWindows() = %v, want weekly_scoped:region:eu — aaa sorts first and names nothing", scopedNames(ws))
	}
	if n := s.UnnamableLimits(); n != 0 {
		t.Errorf("UnnamableLimits() = %d, want 0 — the entry did produce a window", n)
	}
}

// Counting on no reading, and counting an entry of a kind that is not a weekly
// window, are the two ways this number lies in the quiet direction and the loud
// one.
func TestUnnamableLimitsCountsOnlyWeeklyEntriesAndSurvivesNoSnapshot(t *testing.T) {
	var none *Snapshot
	if n := none.UnnamableLimits(); n != 0 {
		t.Errorf("UnnamableLimits() on a nil snapshot = %d, want 0", n)
	}
	s := &Snapshot{Limits: []Limit{LimitFor(LimitInput{Kind: "one_time_grant"})}}
	if n := s.UnnamableLimits(); n != 0 {
		t.Errorf("UnnamableLimits() = %d, want 0 — an entry of an unknown kind is not a weekly window this build lost", n)
	}
}
