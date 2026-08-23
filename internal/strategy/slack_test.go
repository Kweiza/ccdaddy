package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// thr is opts()'s bundle: one threshold of 80 for every window, which is what
// every headroom test written before the per-window table measures against.
func thr() Thresholds { return opts().Thresholds() }

// perWindow is opts() with a threshold table, which is the only configuration
// that can make slack and raw headroom disagree.
func perWindow(table map[usage.WindowName]float64) Options {
	o := opts()
	o.WindowThreshold = table
	return o
}

// ---- the invariance gate ----------------------------------------------------

// THE gate on the whole axis change: with no per-window table, every window sits
// on one threshold, so
//
//	min(slack) = 80 - max(utilization) = Headroom.Pct - 20
//
// and the order is the old order shifted by a constant. The expected orders
// below are written out by hand from the OLD rule — three tiers, then most
// headroom, then uuid — so a slack axis that reordered anything would show up
// here rather than being reproduced by the comparator under test.
func TestAtDefaultConfigurationTheSlackAxisReproducesTheHeadroomOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cands    []Candidate
		want     []string
		wantOver bool
	}{
		{
			name: "someone has room",
			cands: []Candidate{
				sub("u1-tight", snap(win(70, time.Hour), win(20, 48*time.Hour))),
				sub("u2-roomy", snap(win(5, time.Hour), win(10, 48*time.Hour))),
				sub("u3-middle", snap(win(40, time.Hour), win(10, 48*time.Hour))),
			},
			want: []string{"u2-roomy", "u3-middle", "u1-tight"},
		},
		{
			name: "an unreadable account sits between room and exhaustion",
			cands: []Candidate{
				sub("ccc-spent", snap(win(99, time.Hour), win(99, 48*time.Hour))),
				sub("aaa-unread", nil),
				sub("zzz-roomy", snap(win(5, time.Hour), win(5, 48*time.Hour))),
			},
			want: []string{"zzz-roomy", "aaa-unread", "ccc-spent"},
		},
		{
			name: "exactly on the threshold is not over it",
			cands: []Candidate{
				sub("aaa-on-the-line", snap(win(80, time.Hour), win(80, 48*time.Hour))),
				sub("zzz-a-hair-over", snap(win(80.1, time.Hour), win(80.1, 48*time.Hour))),
			},
			want: []string{"aaa-on-the-line", "zzz-a-hair-over"},
		},
		{
			name: "every account spent, so recovery orders them",
			cands: []Candidate{
				sub("p-back-soon", snap(win(99, 8*time.Minute), win(50, 48*time.Hour))),
				sub("q-back-later", snap(win(85, 40*time.Hour), win(50, 48*time.Hour))),
			},
			want:     []string{"p-back-soon", "q-back-later"},
			wantOver: true,
		},
		{
			name: "identical accounts fall to the uuid",
			cands: []Candidate{
				sub("ccc", snap(win(30, time.Hour), win(30, 48*time.Hour))),
				sub("aaa", snap(win(30, time.Hour), win(30, 48*time.Hour))),
				sub("bbb", snap(win(30, time.Hour), win(30, 48*time.Hour))),
			},
			want: []string{"aaa", "bbb", "ccc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Rank(tc.cands, opts())
			eq(t, order(r), tc.want)
			if r.AllOverThreshold != tc.wantOver {
				t.Errorf("AllOverThreshold = %v, want %v", r.AllOverThreshold, tc.wantOver)
			}
			// And the arithmetic the invariance rests on, on every row that
			// could be read: one threshold of 80 makes slack a constant shift.
			for _, x := range r.Order {
				if !x.Headroom.Known {
					continue
				}
				if x.Headroom.Slack != x.Headroom.Pct-20 {
					t.Errorf("%s: Slack = %v, want Pct-20 = %v", x.UUID, x.Headroom.Slack, x.Headroom.Pct-20)
				}
				if x.Headroom.Threshold != DefaultThreshold {
					t.Errorf("%s: Threshold = %v, want %v", x.UUID, x.Headroom.Threshold, DefaultThreshold)
				}
			}
		})
	}
}

// The other half of the invariance: Spent at one threshold is the old
// utilization-over-threshold test, boundary included. 80 exactly is NOT over.
func TestAtDefaultConfigurationSpentIsTheOldThresholdTest(t *testing.T) {
	for _, util := range []float64{0, 50, 79.9, 80, 80.1, 99, 140} {
		h := HeadroomOf(&usage.Snapshot{FiveHour: win(util, time.Hour)}, thr())
		spent, known := Spent(h)
		if !known {
			t.Fatalf("utilization %v: Known = false", util)
		}
		if want := util > DefaultThreshold; spent != want {
			t.Errorf("utilization %v: Spent = %v, want %v", util, spent, want)
		}
	}
}

// An account that could not be read is neither spent nor unspent. Folding that
// into a boolean is the bug that left cswap parked on the account that reset
// last.
func TestSpentStaysThreeValued(t *testing.T) {
	spent, known := Spent(HeadroomOf(nil, thr()))
	if spent || known {
		t.Errorf("Spent(unreadable) = %v, %v; want false, false", spent, known)
	}
}

// ---- what the axis is for ---------------------------------------------------

// The case raw headroom gets wrong. A weekly window on a threshold of 60 and a
// five-hour one on 85 are not comparable through "100 minus utilization": the
// account with more raw headroom is the one closer to tripping a limit.
func TestSlackRanksByDistanceToEachWindowsOwnThreshold(t *testing.T) {
	o := perWindow(map[usage.WindowName]float64{
		usage.WindowFiveHour: 85,
		usage.WindowSevenDay: 60,
	})
	// a: 45 points of raw headroom, 5 points of slack — five from its floor.
	// z: 30 points of raw headroom, 15 points of slack.
	// The uuids run opposite to the answer, so the tie-break cannot produce it.
	cands := []Candidate{
		sub("a-more-headroom", snap(win(10, time.Hour), win(55, 48*time.Hour))),
		sub("z-more-slack", snap(win(70, time.Hour), win(20, 48*time.Hour))),
	}

	if h := HeadroomOf(cands[0].Usage, thr()); h.Pct != 45 {
		t.Fatalf("raw headroom = %v, want 45 — the control for this test", h.Pct)
	}
	r := Rank(cands, o)
	eq(t, order(r), []string{"z-more-slack", "a-more-headroom"})
	if r.Order[1].Headroom.Slack != 5 || r.Order[1].Headroom.Binding != usage.WindowSevenDay {
		t.Errorf("Slack = %v on %v, want 5 on seven_day",
			r.Order[1].Headroom.Slack, r.Order[1].Headroom.Binding)
	}
	if r.Order[1].Headroom.Threshold != 60 {
		t.Errorf("Threshold = %v, want the seven_day entry of 60", r.Order[1].Headroom.Threshold)
	}
}

// Spent is an OR over the windows: either side going over its own threshold
// marks the account, whatever the other side says.
func TestSpentIsAnOrOverTheWindows(t *testing.T) {
	o := perWindow(map[usage.WindowName]float64{
		usage.WindowFiveHour: 85,
		usage.WindowSevenDay: 60,
	})
	for _, tc := range []struct {
		name             string
		fiveHour, weekly float64
		want             bool
	}{
		{"weekly over, five-hour under", 10, 65, true},
		{"five-hour over, weekly under", 90, 20, true},
		{"neither over", 80, 55, false},
		{"both over", 90, 65, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := HeadroomFor(snap(win(tc.fiveHour, time.Hour), win(tc.weekly, 48*time.Hour)), "", o.Thresholds())
			spent, known := Spent(h)
			if !known {
				t.Fatal("Known = false")
			}
			if spent != tc.want {
				t.Errorf("Spent = %v, want %v (slack %v on %v)", spent, tc.want, h.Slack, h.Binding)
			}
		})
	}
}

// ---- the weekly floor -------------------------------------------------------

// A blown weekly window is the one that will not come back for days, so it is
// what has to clear before the account is usable again. Ranking the account on
// its five-hour rollover hands the session to an account that hard-limits on the
// first prompt.
func TestABlownWeeklyFloorIsWhatHasToClear(t *testing.T) {
	o := perWindow(map[usage.WindowName]float64{
		usage.WindowFiveHour: 85,
		usage.WindowSevenDay: 60,
	})
	s := snap(win(95, 10*time.Minute), win(65, 40*time.Hour))

	h := HeadroomFor(s, "", o.Thresholds())
	// The ORDERING window is still the five-hour one: it has the least slack,
	// and Slack is what puts this account against the others.
	if h.Binding != usage.WindowFiveHour || h.Pct != 5 || h.Slack != -10 {
		t.Fatalf("Headroom = %+v; want the five-hour window at 5 left and -10 slack", h)
	}
	// The REPORTED window is the weekly floor. Ordering and reporting are two
	// questions: what is tightest right now, and what will still be tight in two
	// days.
	if !h.HasFloor || h.Floor != usage.WindowSevenDay {
		t.Fatalf("Floor = %v (has %v), want seven_day", h.Floor, h.HasFloor)
	}

	r := Rank([]Candidate{sub("a", s)}, o)
	if !r.Order[0].HasRecovery {
		t.Fatal("HasRecovery = false")
	}
	if want := now.Add(40 * time.Hour); !r.Order[0].RecoversAt.Equal(want) {
		t.Errorf("RecoversAt = %v, want the weekly floor's reset at %v", r.Order[0].RecoversAt, want)
	}
	if r.Order[0].ReturnsInsideHorizon {
		t.Error("ReturnsInsideHorizon = true; the five-hour window returns in ten minutes but the weekly floor does not")
	}
}

// A weekly window UNDER its threshold is not a floor, so the five-hour rollover
// is the recovery again.
func TestAWeeklyWindowThatIsNotBlownIsNotAFloor(t *testing.T) {
	s := snap(win(95, 10*time.Minute), win(50, 40*time.Hour))

	h := HeadroomOf(s, thr())
	if h.HasFloor {
		t.Errorf("Floor = %v; a weekly window at 50%% is not a floor anything is past", h.Floor)
	}
	r := Rank([]Candidate{sub("a", s)}, opts())
	if want := now.Add(10 * time.Minute); !r.Order[0].RecoversAt.Equal(want) {
		t.Errorf("RecoversAt = %v, want the five-hour reset at %v", r.Order[0].RecoversAt, want)
	}
}

// Both sides blown: the floor is the WEEKLY one even though the five-hour window
// is further past its threshold, because that is the one that does not come back
// for days.
func TestTheFloorIsWeeklyEvenWhenTheFiveHourWindowIsFurtherOver(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour:     win(99, 10*time.Minute),
		SevenDay:     win(85, 40*time.Hour),
		SevenDayOpus: win(95, 30*time.Hour),
	}

	h := HeadroomOf(s, thr())
	if h.Binding != usage.WindowFiveHour {
		t.Errorf("Binding = %v, want five_hour — it has the least slack", h.Binding)
	}
	// Among the blown weeklies the one with the LEAST slack is the floor.
	if !h.HasFloor || h.Floor != usage.WindowSevenDayOpus {
		t.Errorf("Floor = %v, want seven_day_opus at 95%%", h.Floor)
	}
}

// The floor never moves an account in the ranking. The two spent accounts here
// are ordered by slack alone, and the one carrying a blown weekly floor is the
// one with MORE slack, so a floor that leaked into the comparator would flip
// them.
//
// The third account is not scenery: it has room, which is what keeps the pass in
// ModeHeadroom. With every account spent the pass would switch to ModeRecovery
// and this would be measuring the recovery key instead.
func TestTheWeeklyFloorDoesNotReorderThePool(t *testing.T) {
	cands := []Candidate{
		// Least slack: -19 on five_hour. No weekly floor at all.
		sub("aaa-tightest", snap(win(99, time.Hour), win(10, 48*time.Hour))),
		// More slack: -5 on seven_day, which is also its floor.
		sub("zzz-weekly-floor", snap(win(10, time.Hour), win(85, 48*time.Hour))),
		sub("m-has-room", snap(win(10, time.Hour), win(10, 48*time.Hour))),
	}

	r := Rank(cands, opts())
	if r.Mode != ModeHeadroom {
		t.Fatalf("Mode = %v, want ModeHeadroom", r.Mode)
	}
	eq(t, order(r), []string{"m-has-room", "zzz-weekly-floor", "aaa-tightest"})
}

// ---- the threshold bundle ---------------------------------------------------

// A zero threshold read literally makes every account spent, which opens the
// credit gate. The zero value must not be the one that starts spending money,
// and neither must a zero left in the table.
func TestTheBundleDefaultsRatherThanReadingAZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		t    Thresholds
		w    usage.WindowName
		want float64
	}{
		{"the zero bundle", Thresholds{}, usage.WindowFiveHour, DefaultThreshold},
		{"the default covers every window", Thresholds{Default: 60}, usage.WindowSevenDay, 60},
		{"an entry wins for its own window", Thresholds{
			Default:   60,
			PerWindow: map[usage.WindowName]float64{usage.WindowFiveHour: 85},
		}, usage.WindowFiveHour, 85},
		{"and only for its own window", Thresholds{
			Default:   60,
			PerWindow: map[usage.WindowName]float64{usage.WindowFiveHour: 85},
		}, usage.WindowSevenDay, 60},
		{"a zero entry is an omission, not 'spent at any usage'", Thresholds{
			PerWindow: map[usage.WindowName]float64{usage.WindowFiveHour: 0},
		}, usage.WindowFiveHour, DefaultThreshold},
		{"a scoped window is keyed by its wire name", Thresholds{
			PerWindow: map[usage.WindowName]float64{"weekly_scoped:model:Opus 4.5": 40},
		}, "weekly_scoped:model:Opus 4.5", 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.t.For(tc.w); got != tc.want {
				t.Errorf("For(%q) = %v, want %v", tc.w, got, tc.want)
			}
		})
	}
}

func TestTheCreditThresholdDefaultsTheSameWay(t *testing.T) {
	if got := (Thresholds{}).CreditThreshold(); got != DefaultCreditThreshold {
		t.Errorf("the zero bundle's CreditThreshold() = %v, want %v", got, DefaultCreditThreshold)
	}
	if got := (Thresholds{Credit: 50}).CreditThreshold(); got != 50 {
		t.Errorf("CreditThreshold() = %v, want 50", got)
	}
}

// Options.Thresholds is the one crossing between the knobs and the bundle. A
// field dropped here is a configured threshold the engine silently ignores.
func TestOptionsCarryEveryThresholdIntoTheBundle(t *testing.T) {
	o := Options{
		Threshold:       60,
		WindowThreshold: map[usage.WindowName]float64{usage.WindowSevenDay: 40},
		CreditThreshold: 90,
	}

	b := o.Thresholds()
	if got := b.For(usage.WindowFiveHour); got != 60 {
		t.Errorf("For(five_hour) = %v, want the scalar 60", got)
	}
	if got := b.For(usage.WindowSevenDay); got != 40 {
		t.Errorf("For(seven_day) = %v, want the table's 40", got)
	}
	if got := b.CreditThreshold(); got != 90 {
		t.Errorf("CreditThreshold() = %v, want 90", got)
	}
}

// ---- the axis split the margins run on --------------------------------------

// The additive margin runs on SLACK. These two accounts are thirty-five points
// apart on raw headroom, which clears the ten-point margin, and five points
// apart on slack, which does not — so the axis is the whole of the answer.
func TestTheHysteresisMarginRunsOnSlack(t *testing.T) {
	// a is on a five-hour window, b on a weekly one, and the two thresholds are
	// thirty points apart. That gap is what separates the axes.
	cands := []Candidate{hr("a", 30, time.Hour), weekly("b", 65, 48*time.Hour)}

	// The control: one threshold for every window, and the move happens.
	want(t, Decide(cands, opts(), Config{}, NewState(), "a"), ActionSwitch, ReasonBetterTarget, "b")

	o := perWindow(map[usage.WindowName]float64{
		usage.WindowFiveHour: 90,
		usage.WindowSevenDay: 60,
	})
	p := Decide(cands, o, Config{}, NewState(), "a")
	if p.Result.Order[0].UUID != "b" {
		t.Fatalf("order = %v, want b first — the margin is only reached for a candidate that outranks", order(p.Result))
	}
	want(t, p, ActionStay, ReasonHysteresis, "")
}

// The ratio stays on raw Pct, and this is the seam that leaves: an account one
// point from a tight weekly floor still has forty points of raw headroom, so a
// candidate that clears the additive margin can fail the ratio and the engine
// stays. Documented rather than fixed — a ratio is not shift-invariant, and
// moving it would change what the engine does for a user who configured nothing.
func TestTheHeadroomRatioStaysOnRawHeadroomAndLeavesASeam(t *testing.T) {
	o := perWindow(map[usage.WindowName]float64{usage.WindowSevenDay: 60})
	// a: 59% used, so one point of slack under a floor of 60 — and 41 points of
	// raw headroom. b: 20 points of slack, 60 points of raw headroom.
	cands := []Candidate{weekly("a", 41, 48*time.Hour), weekly("b", 60, 48*time.Hour)}

	p := Decide(cands, o, Config{}, NewState(), "a")
	if p.Result.Order[0].UUID != "b" {
		t.Fatalf("order = %v, want b first", order(p.Result))
	}
	// Nineteen points of slack clears the ten-point additive margin outright, so
	// the refusal can only be the ratio: 60 is not twice 41.
	want(t, p, ActionStay, ReasonHeadroomRatio, "")
}
