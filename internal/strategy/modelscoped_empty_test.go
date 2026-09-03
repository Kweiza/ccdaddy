package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The shape this file is about, taken from a real fleet: every account's Fable
// week was gone while its all-model weekly still held a fifth, and ccdad filed
// all four in the empty tier. Nobody could be handed work, the anti-flap gate
// waved every margin through to get off an "empty" account, and the engine
// ping-ponged between the same two accounts every two minutes for half an hour
// while a fifth of four weeks went unspent.
//
// account4 is that account, in its own numbers: five_hour fresh, all-model
// weekly at 80%, Fable at 100%.
func account4() *usage.Snapshot {
	return &usage.Snapshot{
		FiveHour: win(0, 4*time.Hour),
		SevenDay: win(80, 46*time.Hour),
		Limits:   []usage.Limit{scoped("Fable", "", 100, 46*time.Hour)},
	}
}

// ---- the empty test asks what any model can spend ---------------------------

func TestABlownModelScopedCapDoesNotEmptyAnAccountThatCanStillServeOtherModels(t *testing.T) {
	h := HeadroomOf(account4(), thr())

	if h.MinPct != 0 {
		t.Errorf("MinPct = %v, want 0 — the Fable week really is gone and MinPct still says so", h.MinPct)
	}
	if h.MinAnyModelPct != 20 {
		t.Errorf("MinAnyModelPct = %v, want 20 — the all-model weekly is what a non-Fable prompt spends", h.MinAnyModelPct)
	}
	if h.MinAnyModelWindow != usage.WindowSevenDay {
		t.Errorf("MinAnyModelWindow = %q, want seven_day", h.MinAnyModelWindow)
	}
	empty, known := OutOfQuota(h)
	if !known {
		t.Fatal("OutOfQuota known = false, want true")
	}
	if empty {
		t.Error("OutOfQuota = true, want false — an account holding a fifth of its week is not empty")
	}
}

// Spent is the other half of the pair and deliberately does NOT move: a blown
// sub-cap is still past a line, so the engine still leaves the account in good
// time rather than walking a Fable session into a hard limit.
func TestABlownModelScopedCapStillMakesTheAccountSpent(t *testing.T) {
	spent, known := Spent(HeadroomOf(account4(), thr()))
	if !known {
		t.Fatal("Spent known = false, want true")
	}
	if !spent {
		t.Error("Spent = false, want true — MinPct is zero and a blown week is past its line")
	}
}

// The fixed per-model pair is model-scoped for the same reason the synthetic
// names are, and FixedWindowFamily is the one place that is spelled.
func TestABlownSevenDayOpusDoesNotEmptyAnAccount(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour:     win(10, time.Hour),
		SevenDay:     win(50, 48*time.Hour),
		SevenDayOpus: win(100, 48*time.Hour),
	}

	h := HeadroomOf(s, thr())
	if h.MinAnyModelPct != 50 {
		t.Errorf("MinAnyModelPct = %v, want 50", h.MinAnyModelPct)
	}
	if empty, _ := OutOfQuota(h); empty {
		t.Error("OutOfQuota = true, want false — seven_day_opus caps one family")
	}
}

// ---- what still empties an account ------------------------------------------

// A SURFACE-scoped cap is not escaped by changing model: Claude Code is itself a
// surface and the wire never says which surface name is this client's own.
func TestABlownSurfaceScopedCapStillEmptiesTheAccount(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		SevenDay: win(50, 48*time.Hour),
		Limits:   []usage.Limit{scoped("", "Claude Code", 100, 48*time.Hour)},
	}

	empty, known := OutOfQuota(HeadroomOf(s, thr()))
	if !known {
		t.Fatal("OutOfQuota known = false, want true")
	}
	if !empty {
		t.Error("OutOfQuota = false, want true — a surface cap is not one a model choice dodges")
	}
}

func TestABlownAllModelWeeklyStillEmptiesTheAccount(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		SevenDay: win(100, 48*time.Hour),
		Limits:   []usage.Limit{scoped("Fable", "", 20, 48*time.Hour)},
	}

	if empty, _ := OutOfQuota(HeadroomOf(s, thr())); !empty {
		t.Error("OutOfQuota = false, want true — the all-model weekly is gone")
	}
}

func TestABlownFiveHourWindowStillEmptiesTheAccount(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(100, time.Hour),
		SevenDay: win(30, 48*time.Hour),
	}

	if empty, _ := OutOfQuota(HeadroomOf(s, thr())); !empty {
		t.Error("OutOfQuota = false, want true — five_hour binds whatever runs")
	}
}

// With nothing but a blown sub-cap readable there is no all-model figure at all,
// and the fallback to MinPct is the conservative reading: the blown week is the
// only thing anyone knows about this account.
func TestAnAccountWhoseOnlyReadableWindowIsModelScopedFallsBackToMinPct(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: unread(),
		SevenDay: unread(),
		Limits:   []usage.Limit{scoped("Fable", "", 100, 48*time.Hour)},
	}

	h := HeadroomOf(s, thr())
	if h.MinAnyModelWindow != "" {
		t.Fatalf("MinAnyModelWindow = %q, want empty — no all-model window was readable", h.MinAnyModelWindow)
	}
	empty, known := OutOfQuota(h)
	if !known || !empty {
		t.Errorf("OutOfQuota = (%v, %v), want (true, true)", empty, known)
	}
}

// A Headroom built by hand carries no all-model pair, and every one already in
// the tree means what it meant before this change.
func TestAHandBuiltHeadroomKeepsReadingMinPct(t *testing.T) {
	h := Headroom{Known: true, MinPct: 0, MinWindow: usage.WindowSevenDay}
	if empty, known := OutOfQuota(h); !known || !empty {
		t.Errorf("OutOfQuota = (%v, %v), want (true, true)", empty, known)
	}
}

// ---- the ordering axis lets a blown sub-cap go ------------------------------

// Slack answers "closest to breaching" and the ranking spends it on "who takes
// the next prompt". A sub-cap that is already gone cannot get tighter and says
// nothing about how much work is left, so it stops being the ordering axis.
func TestAnEmptyModelScopedCapStopsBeingTheOrderingAxis(t *testing.T) {
	h := HeadroomOf(account4(), thr())

	if b := bindingOf(t, h); b != string(usage.WindowSevenDay) {
		t.Errorf("Binding = %q, want seven_day — the blown Fable cap is not the ordering axis", b)
	}
	if h.Slack != DefaultThreshold-80 {
		t.Errorf("Slack = %v, want %v — measured on seven_day", h.Slack, DefaultThreshold-80)
	}
}

// It is a guard on the ordering update and not a `continue`, so the blown cap is
// still the window the account is REPORTED against — the column that exists to
// say when it comes back.
func TestAnEmptyModelScopedCapIsStillTheFloor(t *testing.T) {
	h := HeadroomOf(account4(), thr())

	if !h.HasFloor {
		t.Fatal("HasFloor = false, want true")
	}
	if h.Floor != usage.ScopedWindowName(usage.ScopeModel, "Fable") {
		t.Errorf("Floor = %q, want the Fable cap", h.Floor)
	}
}

// EMPTY is the whole test. A sub-cap merely running ahead of its pace still
// bounds the session about to use it, and dropping it there would walk an
// account into a hard limit one prompt later.
func TestAModelScopedCapThatIsMerelyOverItsThresholdStillOrders(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		SevenDay: win(20, 48*time.Hour),
		Limits:   []usage.Limit{scoped("Fable", "", 95, 48*time.Hour)},
	}

	if b := bindingOf(t, HeadroomOf(s, thr())); b != string(usage.ScopedWindowName(usage.ScopeModel, "Fable")) {
		t.Errorf("Binding = %q, want the Fable cap — 95%% is not empty", b)
	}
}

// With no all-model window readable the blown sub-cap is all there is, and
// dropping it from the ordering would report an unbounded account.
func TestAnEmptyModelScopedCapStillOrdersWhenNothingElseIsReadable(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: unread(),
		SevenDay: unread(),
		Limits:   []usage.Limit{scoped("Fable", "", 100, 48*time.Hour)},
	}

	if b := bindingOf(t, HeadroomOf(s, thr())); b != string(usage.ScopedWindowName(usage.ScopeModel, "Fable")) {
		t.Errorf("Binding = %q, want the Fable cap — it is the only reading there is", b)
	}
}
