package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// scoped builds one limits[] entry: a weekly cap the server scopes to a model or
// a surface. A nil resetsIn means the entry reported no reset.
func scoped(model, surface string, pct float64, resetsIn time.Duration) usage.Limit {
	at := now.Add(resetsIn)
	return usage.LimitFor(usage.LimitInput{
		Kind:     "weekly_scoped",
		Model:    model,
		Surface:  surface,
		Percent:  &pct,
		ResetsAt: &at,
	})
}

func bindingOf(t *testing.T, h Headroom) string {
	t.Helper()
	if !h.Known {
		t.Fatal("Headroom.Known = false")
	}
	return string(h.Binding)
}

// ---- limits[] binds ---------------------------------------------------------

// The gap this whole change exists to close: internal/usage parsed limits[] and
// internal/strategy ranked as if it had not. An account whose Fable week is gone
// read as healthy.
func TestHeadroomBindsOnAScopedModelWindow(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		SevenDay: win(20, 48*time.Hour),
		Limits:   []usage.Limit{scoped("Fable", "", 95, 48*time.Hour)},
	}

	h := HeadroomOf(s, thr())
	if h.Pct != 5 {
		t.Errorf("Pct = %v, want 5 — the per-model weekly cap is the window with the least left", h.Pct)
	}
	if b := bindingOf(t, h); b != "weekly_scoped:model:Fable" {
		t.Errorf("Binding = %q, want the scoped window", b)
	}
}

// Claude Code is itself a surface, so a surface weekly cap can be the window
// that binds a ccdad session.
func TestHeadroomBindsOnAScopedSurfaceWindow(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		Limits:   []usage.Limit{scoped("", "Claude Code", 97, 48*time.Hour)},
	}

	h := HeadroomOf(s, thr())
	if h.Pct != 3 {
		t.Errorf("Pct = %v, want 3", h.Pct)
	}
	if b := bindingOf(t, h); b != "weekly_scoped:surface:Claude Code" {
		t.Errorf("Binding = %q, want the surface window", b)
	}
}

// A scoped window whose percent could not be read is a window that exists and
// says nothing. It must not bind at 0% used, which is the shape that would hide
// a spent account behind a "100% left" reading.
func TestAScopedWindowWithNoPercentDoesNotBind(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(30, time.Hour),
		Limits: []usage.Limit{usage.LimitFor(usage.LimitInput{
			Kind: "weekly_scoped", Model: "Fable",
		})},
	}

	h := HeadroomOf(s, thr())
	if h.Pct != 70 {
		t.Errorf("Pct = %v, want 70 — five_hour still binds", h.Pct)
	}
	if b := bindingOf(t, h); b != string(usage.WindowFiveHour) {
		t.Errorf("Binding = %q, want five_hour", b)
	}
}

// ---- --model narrows, and only narrows --------------------------------------

func TestModelNarrowsAwayAnotherFamilysScopedWindow(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		SevenDay: win(40, 48*time.Hour),
		Limits:   []usage.Limit{scoped("Fable", "", 95, 48*time.Hour)},
	}

	if h := HeadroomOf(s, thr()); h.Pct != 5 {
		t.Fatalf("unqualified Pct = %v, want 5 — the control for this test", h.Pct)
	}
	h := HeadroomFor(s, "sonnet", thr())
	if h.Pct != 60 {
		t.Errorf("Pct with --model sonnet = %v, want 60 — a Fable cap does not bind a Sonnet session", h.Pct)
	}
	if b := bindingOf(t, h); b != string(usage.WindowSevenDay) {
		t.Errorf("Binding = %q, want seven_day", b)
	}
}

// The fixed pair is model-scoped too, and it has bound unconditionally since the
// day headroom was written. Naming a model has to reach it, or --model narrows
// half the model axis and leaves the other half.
func TestModelNarrowsTheFixedModelWindowsToo(t *testing.T) {
	s := &usage.Snapshot{
		SevenDay:       win(10, 48*time.Hour),
		SevenDayOpus:   win(99, 48*time.Hour),
		SevenDaySonnet: win(20, 48*time.Hour),
	}

	for _, tc := range []struct {
		model   string
		wantPct float64
		binding usage.WindowName
	}{
		{"", 1, usage.WindowSevenDayOpus},
		{"sonnet", 80, usage.WindowSevenDaySonnet},
		{"Claude Opus 4.5", 1, usage.WindowSevenDayOpus},
		{"claude-opus-5", 1, usage.WindowSevenDayOpus},
	} {
		name := tc.model
		if name == "" {
			name = "unqualified"
		}
		t.Run(name, func(t *testing.T) {
			h := HeadroomFor(s, tc.model, thr())
			if h.Pct != tc.wantPct {
				t.Errorf("Pct = %v, want %v", h.Pct, tc.wantPct)
			}
			if b := bindingOf(t, h); b != string(tc.binding) {
				t.Errorf("Binding = %q, want %q", b, tc.binding)
			}
		})
	}
}

// A surface cap is not a model cap. Naming a model says nothing about it, so it
// keeps binding — the deliberate seam in the --model narrowing rule.
func TestModelNeverNarrowsASurfaceWindow(t *testing.T) {
	s := &usage.Snapshot{
		SevenDay: win(10, 48*time.Hour),
		Limits:   []usage.Limit{scoped("", "Cowork", 95, 48*time.Hour)},
	}

	if h := HeadroomFor(s, "sonnet", thr()); h.Pct != 5 {
		t.Errorf("Pct = %v, want 5 — a surface cap survives --model", h.Pct)
	}
}

// "I do not recognize this model" is not evidence that it is a different one.
// Both halves of that: an unplaceable SCOPE stays, and an unplaceable --model
// narrows nothing at all.
func TestAnUnplaceableNameNarrowsNothing(t *testing.T) {
	t.Run("the window's scope", func(t *testing.T) {
		s := &usage.Snapshot{
			SevenDay: win(10, 48*time.Hour),
			Limits:   []usage.Limit{scoped("Zephyr 1", "", 95, 48*time.Hour)},
		}
		if h := HeadroomFor(s, "sonnet", thr()); h.Pct != 5 {
			t.Errorf("Pct = %v, want 5 — a model ccdad cannot place is not ruled out", h.Pct)
		}
	})
	t.Run("the requested model", func(t *testing.T) {
		s := &usage.Snapshot{
			SevenDay:     win(10, 48*time.Hour),
			SevenDayOpus: win(99, 48*time.Hour),
		}
		if h := HeadroomFor(s, "zephyr", thr()); h.Pct != 1 {
			t.Errorf("Pct = %v, want 1 — an unplaceable --model must not widen the headroom", h.Pct)
		}
	})
}

// --model is monotone: it can only raise an account's headroom. If it could
// lower one, a caller that names a model would be more pessimistic than one that
// does not, and the flag would be a way to make the engine switch MORE.
func TestModelNeverLowersHeadroom(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour:       win(30, time.Hour),
		SevenDay:       win(40, 48*time.Hour),
		SevenDayOpus:   win(99, 48*time.Hour),
		SevenDaySonnet: win(50, 48*time.Hour),
		Limits: []usage.Limit{
			scoped("Fable", "", 96, 48*time.Hour),
			scoped("", "Cowork", 55, 48*time.Hour),
			scoped("Haiku 4.5", "", 80, 48*time.Hour),
		},
	}
	base := HeadroomOf(s, thr())

	for _, m := range append(ModelFamilyNames(), "Claude Opus 4.5", "zephyr") {
		h := HeadroomFor(s, m, thr())
		if !h.Known {
			t.Fatalf("--model %q left the headroom unknown", m)
		}
		if h.Pct < base.Pct {
			t.Errorf("--model %q gives Pct %v, below the unqualified %v", m, h.Pct, base.Pct)
		}
	}
}

func TestModelFamily(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"opus", "opus", true},
		{"Opus", "opus", true},
		{"Claude Opus 4.5", "opus", true},
		{"claude-opus-5", "opus", true},
		{"Sonnet 4.5", "sonnet", true},
		{"Fable", "fable", true},
		{"haiku", "haiku", true},
		{"", "", false},
		{"gpt-5", "", false},
		{"Zephyr 1", "", false},
		// Two families in one string is not a model name at all, and guessing
		// which half was meant is how the wrong cap gets narrowed away.
		{"opus-or-sonnet", "", false},
	} {
		got, ok := ModelFamily(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ModelFamily(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestModelFamilyNamesIsACopy(t *testing.T) {
	got := ModelFamilyNames()
	if len(got) == 0 {
		t.Fatal("ModelFamilyNames() is empty")
	}
	got[0] = "clobbered"
	if _, ok := ModelFamily("opus"); !ok {
		t.Error("mutating the returned slice changed what the matcher knows")
	}
}

// ---- the scoped windows reach the rest of the ranking -----------------------

// Recovery mode ranks on when the BINDING window rolls over. Looking that reset
// up in the fixed five alone leaves an account whose binding is a scoped window
// with no recovery at all — which sorts it behind every account that has one.
func TestRecoveryReadsAScopedBindingWindowsReset(t *testing.T) {
	later := sub("a-later", &usage.Snapshot{FiveHour: win(90, 50*time.Minute)})
	sooner := sub("b-sooner", &usage.Snapshot{
		FiveHour: win(85, 3*time.Hour),
		Limits:   []usage.Limit{scoped("Fable", "", 95, 10*time.Minute)},
	})

	res := Rank([]Candidate{later, sooner}, opts())
	if !res.AllOverThreshold || res.Mode != ModeRecovery {
		t.Fatalf("Mode = %v, AllOverThreshold = %v; this fixture is the recovery situation",
			res.Mode, res.AllOverThreshold)
	}
	// Alphabetical order is a-later, b-sooner, so the uuid tie-break cannot
	// produce the expected answer on its own.
	eq(t, order(res), []string{"b-sooner", "a-later"})

	if !res.Order[0].HasRecovery {
		t.Fatal("the account whose binding window is a scoped one has no recovery")
	}
	if want := now.Add(10 * time.Minute); !res.Order[0].RecoversAt.Equal(want) {
		t.Errorf("RecoversAt = %v, want %v", res.Order[0].RecoversAt, want)
	}
}

// consume-first spends perishable quota before it expires, and a per-model
// weekly cap is exactly that.
func TestConsumeFirstSeesAScopedWeeklyReset(t *testing.T) {
	late := sub("a-late", snap(win(10, time.Hour), win(10, 100*time.Hour)))
	soon := sub("b-soon", &usage.Snapshot{
		FiveHour: win(10, time.Hour),
		SevenDay: win(10, 200*time.Hour),
		Limits:   []usage.Limit{scoped("Fable", "", 10, 10*time.Hour)},
	})

	o := opts()
	o.Strategy = StrategyConsumeFirst
	res := Rank([]Candidate{late, soon}, o)
	if res.Mode != ModeConsumeFirst {
		t.Fatalf("Mode = %v", res.Mode)
	}
	eq(t, order(res), []string{"b-soon", "a-late"})
}

// --model narrows the pass consume-first ranks on too, or the flag would mean
// one thing under one strategy and another under the other.
func TestConsumeFirstNarrowsWithTheModel(t *testing.T) {
	fable := sub("a-fable", &usage.Snapshot{
		SevenDay: win(10, 200*time.Hour),
		Limits:   []usage.Limit{scoped("Fable", "", 10, 10*time.Hour)},
	})
	sonnet := sub("b-sonnet", &usage.Snapshot{SevenDay: win(10, 100*time.Hour)})

	o := opts()
	o.Strategy = StrategyConsumeFirst
	if got := order(Rank([]Candidate{fable, sonnet}, o)); got[0] != "a-fable" {
		t.Fatalf("unqualified order = %v, want a-fable first — the control for this test", got)
	}

	o.Model = "sonnet"
	eq(t, order(Rank([]Candidate{fable, sonnet}, o)), []string{"b-sonnet", "a-fable"})
}

// The end of the wire: Options.Model reaches the order Decide walks.
func TestRankNarrowsTheWholePassWithTheModelOption(t *testing.T) {
	// z-opus-spent has no Sonnet problem at all; its Opus week is gone.
	opusSpent := sub("z-opus-spent", &usage.Snapshot{
		FiveHour:       win(10, time.Hour),
		SevenDay:       win(10, 48*time.Hour),
		SevenDayOpus:   win(99, 48*time.Hour),
		SevenDaySonnet: win(5, 48*time.Hour),
	})
	modest := sub("a-modest", &usage.Snapshot{
		FiveHour:       win(50, time.Hour),
		SevenDay:       win(50, 48*time.Hour),
		SevenDaySonnet: win(40, 48*time.Hour),
	})
	cands := []Candidate{opusSpent, modest}

	// Control: unqualified, the spent Opus week binds and sinks the account.
	res := Rank(cands, opts())
	eq(t, order(res), []string{"a-modest", "z-opus-spent"})

	// Named: the Opus cap is not this session's, and the account with the most
	// Sonnet room wins. Alphabetical order is the opposite of this answer.
	o := opts()
	o.Model = "sonnet"
	res = Rank(cands, o)
	eq(t, order(res), []string{"z-opus-spent", "a-modest"})
	if res.Order[0].Headroom.Pct != 90 {
		t.Errorf("Pct = %v, want 90 — five_hour and seven_day still bind", res.Order[0].Headroom.Pct)
	}
}

// SubscriptionExhausted opens the credit gate, so it has to ask the same
// question the ranking does: an account that is only out of ANOTHER model's
// quota is not exhausted for this session, and spending money on it would be
// the gate failing open.
func TestSubscriptionExhaustedNarrowsWithTheModel(t *testing.T) {
	cands := []Candidate{sub("u-1", &usage.Snapshot{
		FiveHour:       win(10, time.Hour),
		SevenDayOpus:   win(99, 48*time.Hour),
		SevenDaySonnet: win(10, 48*time.Hour),
	})}

	if !SubscriptionExhausted(cands, opts()) {
		t.Error("unqualified: the spent Opus week is over threshold, so the pool is exhausted")
	}
	o := opts()
	o.Model = "sonnet"
	if SubscriptionExhausted(cands, o) {
		t.Error("with --model sonnet the account has 90% left; the credit gate must stay shut")
	}
}
