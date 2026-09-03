package usage

import (
	"encoding/json"
	"testing"
	"time"
)

func pctOf(v float64) *float64 { return &v }

// A Codex window's length arrives in the reading rather than out of a table:
// the endpoint sends limit_window_seconds and the plans do not agree on it.
func TestAWindowCanCarryItsOwnLength(t *testing.T) {
	reset := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	w := NewWindowWithLength(pctOf(14), &reset, 30*24*time.Hour)

	if !w.Present {
		t.Fatal("NewWindowWithLength() built an absent window")
	}
	got, ok := w.Length()
	if !ok {
		t.Fatal("Length() reported absent for a window built with one")
	}
	if got != 30*24*time.Hour {
		t.Errorf("Length() = %s, want 720h", got)
	}
	if p, ok := w.Percent(); !ok || p != 14 {
		t.Errorf("Percent() = %v, %v; want 14, true", p, ok)
	}
}

// A length is unknown, never zero. Zero divides the pace arithmetic.
func TestAWindowWithNoLengthReportsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    Window
	}{
		{"the zero window", Window{}},
		{"a window built without one", NewWindow(pctOf(50), nil)},
		{"a window built with a zero length", NewWindowWithLength(pctOf(50), nil, 0)},
		{"a window built with a negative length", NewWindowWithLength(pctOf(50), nil, -time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d, ok := tc.w.Length(); ok {
				t.Errorf("Length() = %s, true; want unknown", d)
			}
		})
	}
}

// The window's own length WINS. The table answers only for the names it has,
// and it has none for either codex window.
func TestWindowLengthOfPrefersTheReadingOverTheTable(t *testing.T) {
	if d, ok := WindowLengthOf(WindowCodexPrimary, NewWindowWithLength(nil, nil, 3*time.Hour)); !ok || d != 3*time.Hour {
		t.Errorf("WindowLengthOf(codex_primary, len=3h) = %s, %v; want 3h, true", d, ok)
	}
	if d, ok := WindowLengthOf(WindowFiveHour, NewWindowWithLength(nil, nil, 3*time.Hour)); !ok || d != 3*time.Hour {
		t.Errorf("WindowLengthOf(five_hour, len=3h) = %s, %v; want the reading's 3h, true", d, ok)
	}
	if d, ok := WindowLengthOf(WindowFiveHour, Window{}); !ok || d != 5*time.Hour {
		t.Errorf("WindowLengthOf(five_hour, no length) = %s, %v; want the table's 5h, true", d, ok)
	}
	if _, ok := WindowLengthOf(WindowCodexPrimary, Window{}); ok {
		t.Error("WindowLengthOf(codex_primary, no length) answered; there is no table entry to fall back on")
	}
	// cinder_cove keeps its refusal: its resets_at is an expiry, so a length
	// for it would invent an endless series of grants that never arrive.
	if _, ok := WindowLengthOf(WindowCinderCove, Window{}); ok {
		t.Error("WindowLengthOf(cinder_cove, no length) answered")
	}
}

func TestIsWeeklyOfReadsTheLengthWhenThereIsOne(t *testing.T) {
	if !IsWeeklyOf(WindowCodexPrimary, NewWindowWithLength(nil, nil, 30*24*time.Hour)) {
		t.Error("a 30-day codex window is not weekly-or-longer")
	}
	if IsWeeklyOf(WindowCodexPrimary, NewWindowWithLength(nil, nil, 5*time.Hour)) {
		t.Error("a five-hour codex window read as weekly")
	}
	// With no length the name is still the answer, which is what keeps every
	// Claude call site unchanged.
	if !IsWeeklyOf(WindowSevenDay, Window{}) {
		t.Error("seven_day stopped being weekly")
	}
	if IsWeeklyOf(WindowFiveHour, Window{}) {
		t.Error("five_hour became weekly")
	}
}

// A pace reading needs a length, and a codex window's is in the reading. Without
// this the PACE cell is blank on every Codex row.
func TestPaceAndExpectedPctUseTheReadingsOwnLength(t *testing.T) {
	length := 10 * time.Hour
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	reset := now.Add(5 * time.Hour) // exactly half the window has run
	w := NewWindowWithLength(pctOf(50), &reset, length)

	pct, ok := ExpectedPct(WindowCodexPrimary, w, now)
	if !ok {
		t.Fatal("ExpectedPct() reported absent for a window carrying its own length")
	}
	if pct != 50 {
		t.Errorf("ExpectedPct() = %v, want 50", pct)
	}
	if p := PaceOf(WindowCodexPrimary, w, now); p.Reason == PaceNoWindowLength {
		t.Error("PaceOf() found no window length on a window that carries one")
	}
}

// The ranked list and the settable list are built separately, and both have to
// know the codex names or a threshold on one governs nothing.
func TestTheCodexWindowsAreRankedAndSettable(t *testing.T) {
	present := NewWindowWithLength(pctOf(1), nil, time.Hour)
	ranked := (&Snapshot{CodexPrimary: present, CodexSecondary: present}).RateLimitWindows()
	names := map[WindowName]bool{}
	for _, w := range ranked {
		names[w.Name] = true
	}
	if !names[WindowCodexPrimary] || !names[WindowCodexSecondary] {
		t.Errorf("RateLimitWindows() = %v, want both codex windows", names)
	}
	for _, n := range []WindowName{WindowCodexPrimary, WindowCodexSecondary} {
		if err := ValidWindowName(n); err != nil {
			t.Errorf("ValidWindowName(%q) = %v", n, err)
		}
	}
}

// An ABSENT codex window is not appended. A Claude account would otherwise rank
// two windows it never reported, and every Claude ranking test would move.
func TestAnAbsentCodexWindowIsNotRanked(t *testing.T) {
	if got := len((&Snapshot{}).RateLimitWindows()); got != 5 {
		t.Errorf("a snapshot with no codex reading ranks %d windows, want the fixed five", got)
	}
	if got := len((&Snapshot{CodexPrimary: NewWindow(nil, nil)}).RateLimitWindows()); got != 6 {
		t.Errorf("one codex window present ranks %d, want six", got)
	}
}

// The disk is the cache, and a length that does not survive it is a PACE cell
// that goes blank on the next command.
func TestTheCodexWindowsRoundTripThroughTheCodec(t *testing.T) {
	reset := time.Date(2026, 9, 30, 12, 0, 0, 30*1e6, time.UTC)
	in := &Snapshot{
		CodexPrimary:   NewWindowWithLength(pctOf(14), &reset, 30*24*time.Hour),
		CodexSecondary: NewWindowWithLength(pctOf(2.5), nil, 5*time.Hour),
	}

	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("the encoded form is not an object: %v", err)
	}
	for _, k := range []string{"codex_primary", "codex_secondary"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("the encoded snapshot dropped %q", k)
		}
	}

	var out Snapshot
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if d, ok := out.CodexPrimary.Length(); !ok || d != 30*24*time.Hour {
		t.Errorf("CodexPrimary.Length() = %s, %v after the round trip", d, ok)
	}
	if p, ok := out.CodexPrimary.Percent(); !ok || p != 14 {
		t.Errorf("CodexPrimary.Percent() = %v, %v", p, ok)
	}
	if at, ok := out.CodexPrimary.Reset(); !ok || !at.Equal(reset) {
		t.Errorf("CodexPrimary.Reset() = %s, %v, want %s — the milliseconds must survive the disk", at, ok, reset)
	}
	if d, ok := out.CodexSecondary.Length(); !ok || d != 5*time.Hour {
		t.Errorf("CodexSecondary.Length() = %s, %v", d, ok)
	}
	if _, ok := out.CodexSecondary.Reset(); ok {
		t.Error("an unreported reset came back as a value")
	}
}

// An absent window encodes as an explicit null, the way the other six do, so
// the document has a fixed key set whichever provider wrote it.
func TestAnAbsentCodexWindowEncodesAsNull(t *testing.T) {
	encoded, err := json.Marshal(&Snapshot{FiveHour: NewWindow(pctOf(1), nil)})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"codex_primary", "codex_secondary"} {
		if got := string(keys[k]); got != "null" {
			t.Errorf("%s = %s, want null", k, got)
		}
	}

	var out Snapshot
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if out.CodexPrimary.Present || out.CodexSecondary.Present {
		t.Error("a null codex window came back present")
	}
}
