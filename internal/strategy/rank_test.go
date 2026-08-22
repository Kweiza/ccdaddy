package strategy

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

var now = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func win(pct float64, resetsIn time.Duration) usage.Window {
	at := now.Add(resetsIn)
	return usage.NewWindow(&pct, &at)
}

// unread is a window the response carried but could not fill in.
func unread() usage.Window { return usage.NewWindow(nil, nil) }

func snap(five, sevenDay usage.Window) *usage.Snapshot {
	return &usage.Snapshot{FiveHour: five, SevenDay: sevenDay}
}

func sub(uuid string, s *usage.Snapshot) Candidate {
	return Candidate{UUID: uuid, Kind: identity.KindSubscription, Usage: s}
}

func opts() Options {
	return Options{Now: now, Threshold: DefaultThreshold, Horizon: DefaultRecoveryHorizon}
}

func order(r Result) []string {
	out := make([]string, 0, len(r.Order))
	for _, x := range r.Order {
		out = append(out, x.UUID)
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// ---- headroom --------------------------------------------------------------

// Headroom is not one number. An account has several windows and the BINDING one
// is the minimum: ranking on five_hour alone hands work to an account whose
// weekly Opus quota is gone.
func TestHeadroomIsTheMinimumAcrossWindows(t *testing.T) {
	s := &usage.Snapshot{
		FiveHour:       win(10, time.Hour),
		SevenDay:       win(50, 48*time.Hour),
		SevenDayOpus:   win(97, 48*time.Hour),
		SevenDaySonnet: win(20, 48*time.Hour),
	}

	h := HeadroomOf(s)
	if !h.Known {
		t.Fatal("Known = false")
	}
	if h.Pct != 3 {
		t.Errorf("Pct = %v, want 3 — the binding window is the one with the least left", h.Pct)
	}
	if h.Binding != usage.WindowSevenDayOpus {
		t.Errorf("Binding = %v, want seven_day_opus", h.Binding)
	}
}

func TestHeadroomIgnoresWindowsThatReportedNothing(t *testing.T) {
	s := &usage.Snapshot{FiveHour: win(40, time.Hour), SevenDay: unread()}

	h := HeadroomOf(s)
	if !h.Known || h.Pct != 60 {
		t.Errorf("HeadroomOf() = %+v; want 60 from the one window that answered", h)
	}
	if h.Binding != usage.WindowFiveHour {
		t.Errorf("Binding = %v, want five_hour", h.Binding)
	}
}

func TestHeadroomIsUnknownWhenNoWindowAnswered(t *testing.T) {
	for _, s := range []*usage.Snapshot{nil, {}, {FiveHour: unread(), SevenDay: unread()}} {
		if h := HeadroomOf(s); h.Known {
			t.Errorf("HeadroomOf(%+v).Known = true; an unread account is not an empty one", s)
		}
	}
}

// cinder_cove is a one-time credit, not a rate-limit window, so it must not be
// able to bind. A spent one-time grant would otherwise read as an exhausted
// account forever.
func TestHeadroomIgnoresCinderCove(t *testing.T) {
	s := &usage.Snapshot{FiveHour: win(10, time.Hour), CinderCove: win(100, 30*24*time.Hour)}

	h := HeadroomOf(s)
	if h.Pct != 90 || h.Binding != usage.WindowFiveHour {
		t.Errorf("HeadroomOf() = %+v; cinder_cove is a one-time credit and must not bind the ranking", h)
	}
}

func TestHeadroomKeepsAnOverspentWindowNegative(t *testing.T) {
	s := &usage.Snapshot{FiveHour: win(140, time.Hour)}

	h := HeadroomOf(s)
	if !h.Known || h.Pct != -40 {
		t.Errorf("HeadroomOf() = %+v, want -40 — how far past the limit an account is still orders it", h)
	}
}

// ---- eligibility -----------------------------------------------------------

// Excluded BEFORE ranking, not after: an api-key account has no quota concept at
// all, so there is nothing to compare it on.
func TestRankExcludesAPIKeyAndDisabledAccounts(t *testing.T) {
	cands := []Candidate{
		sub("keep", snap(win(10, time.Hour), win(10, 48*time.Hour))),
		{UUID: "apikey", Kind: identity.KindAPIKey, Usage: snap(win(0, time.Hour), win(0, 48*time.Hour))},
		{UUID: "off", Kind: identity.KindSubscription, Disabled: true, Usage: snap(win(0, time.Hour), win(0, 48*time.Hour))},
	}

	eq(t, order(Rank(cands, opts())), []string{"keep"})
}

// ---- the ordinary situation ------------------------------------------------

func TestRankPrefersTheMostHeadroomWhenSomeoneHasRoom(t *testing.T) {
	cands := []Candidate{
		sub("tight", snap(win(70, time.Hour), win(20, 48*time.Hour))),
		sub("roomy", snap(win(5, time.Hour), win(10, 48*time.Hour))),
		sub("middle", snap(win(40, time.Hour), win(10, 48*time.Hour))),
	}

	r := Rank(cands, opts())
	eq(t, order(r), []string{"roomy", "middle", "tight"})
	if r.Mode != ModeHeadroom {
		t.Errorf("Mode = %v, want ModeHeadroom", r.Mode)
	}
	if r.AllOverThreshold {
		t.Error("AllOverThreshold = true while an account is under threshold")
	}
}

// The cswap bug §7.2 names by name. An account whose usage cannot be read is
// neither over threshold nor under it, so a single expired token must not decide
// the mode — and the unreadable account is still worth trying before one that is
// known to be spent, because a maybe beats a no.
func TestRankPutsAnUnreadableAccountBetweenRoomAndExhaustion(t *testing.T) {
	// The uuids run the opposite way from the expected order on purpose: if the
	// tier were dropped, the tie-break alone would reproduce a passing order.
	cands := []Candidate{
		sub("ccc-spent", snap(win(99, time.Hour), win(99, 48*time.Hour))),
		sub("aaa-unread", nil),
		sub("zzz-roomy", snap(win(5, time.Hour), win(5, 48*time.Hour))),
	}

	r := Rank(cands, opts())
	eq(t, order(r), []string{"zzz-roomy", "aaa-unread", "ccc-spent"})
	if r.AllOverThreshold {
		t.Error("AllOverThreshold = true; one account could not be read, so nobody can say every account is over")
	}
	if r.Mode != ModeHeadroom {
		t.Errorf("Mode = %v; an unreadable account must not tip the engine into recovery mode", r.Mode)
	}
}

func TestRankStaysInHeadroomModeWhenOnlyUnknownsAreLeft(t *testing.T) {
	cands := []Candidate{
		sub("spent", snap(win(99, time.Hour), win(99, 48*time.Hour))),
		sub("unread", nil),
	}

	r := Rank(cands, opts())
	eq(t, order(r), []string{"unread", "spent"})
	if r.AllOverThreshold {
		t.Error("AllOverThreshold = true with an unreadable account in the pool")
	}
}

// ---- every account over threshold ------------------------------------------

// With nothing healthy to land on, sitting still burns the active account to a
// hard limit while a peer that frees up in eight minutes is never tried.
func TestRankPrefersTheSoonestRecoveryWhenEveryoneIsOverThreshold(t *testing.T) {
	cands := []Candidate{
		// Blown hardest, but back in eight minutes.
		sub("soon", snap(win(99, 8*time.Minute), win(99, 48*time.Hour))),
		// Barely over, but not back for two days.
		sub("later", snap(win(85, 40*time.Hour), win(85, 40*time.Hour))),
	}

	r := Rank(cands, opts())
	if r.Mode != ModeRecovery {
		t.Fatalf("Mode = %v, want ModeRecovery", r.Mode)
	}
	if !r.AllOverThreshold {
		t.Error("AllOverThreshold = false with every account over threshold")
	}
	eq(t, order(r), []string{"soon", "later"})
}

// The tier is the whole point. A flat key compares a raw headroom against an
// epoch second — ~1.7e9 against 0-100 — so magnitude decides and the engine
// parks on whatever resets last. An account returning inside the horizon beats
// one that does not, whatever its headroom.
func TestRankTiersRecoveryInsideTheHorizonAboveHeadroom(t *testing.T) {
	cands := []Candidate{
		// Outside the horizon, but far less blown.
		sub("less-blown-later", snap(win(81, 5*time.Hour), win(81, 5*time.Hour))),
		// Inside the horizon, and blown hardest of all.
		sub("wrecked-sooner", snap(win(100, 30*time.Minute), win(100, 30*time.Minute))),
	}

	r := Rank(cands, opts())
	eq(t, order(r), []string{"wrecked-sooner", "less-blown-later"})
}

func TestRankOrdersInsideTheHorizonBySoonestThenByHeadroom(t *testing.T) {
	cands := []Candidate{
		sub("ten-min-blown", snap(win(99, 10*time.Minute), win(99, 48*time.Hour))),
		sub("five-min", snap(win(90, 5*time.Minute), win(90, 48*time.Hour))),
		sub("ten-min-roomier", snap(win(90, 10*time.Minute), win(90, 48*time.Hour))),
	}

	r := Rank(cands, opts())
	eq(t, order(r), []string{"five-min", "ten-min-roomier", "ten-min-blown"})
}

// Inside the horizon the RESET leads and headroom only breaks its ties, so a
// case where the two disagree is the one that pins the order of the key.
func TestRankInsideTheHorizonPrefersTheSoonestEvenWhenItIsMoreBlown(t *testing.T) {
	cands := []Candidate{
		sub("aaa-roomier-later", snap(win(85, 30*time.Minute), win(85, 48*time.Hour))),
		sub("zzz-blown-sooner", snap(win(99, 5*time.Minute), win(99, 48*time.Hour))),
	}

	eq(t, order(Rank(cands, opts())), []string{"zzz-blown-sooner", "aaa-roomier-later"})
}

// The recovery is the binding window's reset, not the first window's. An account
// whose five-hour window is back in a minute is still spent if the weekly Opus
// quota that actually binds it does not return for two days.
func TestRankTakesTheRecoveryFromTheBindingWindow(t *testing.T) {
	cands := []Candidate{
		{UUID: "opus-bound", Kind: identity.KindSubscription, Usage: &usage.Snapshot{
			// five_hour is back almost immediately, but it is not what binds.
			FiveHour:     win(90, time.Minute),
			SevenDayOpus: win(99, 40*time.Hour),
		}},
		{UUID: "back-soon", Kind: identity.KindSubscription, Usage: &usage.Snapshot{
			FiveHour:     win(95, 20*time.Minute),
			SevenDayOpus: win(95, 20*time.Minute),
		}},
	}

	r := Rank(cands, opts())
	if r.Mode != ModeRecovery {
		t.Fatalf("Mode = %v, want ModeRecovery", r.Mode)
	}
	for _, x := range r.Order {
		if x.UUID != "opus-bound" {
			continue
		}
		if x.Headroom.Binding != usage.WindowSevenDayOpus {
			t.Fatalf("Binding = %v, want seven_day_opus", x.Headroom.Binding)
		}
		if x.ReturnsInsideHorizon {
			t.Error("ReturnsInsideHorizon = true; the five-hour window returns soon but the binding one does not")
		}
		if !x.RecoversAt.Equal(now.Add(40 * time.Hour)) {
			t.Errorf("RecoversAt = %s, want the binding window's reset at %s", x.RecoversAt, now.Add(40*time.Hour))
		}
	}
	eq(t, order(r), []string{"back-soon", "opus-bound"})
}

func TestRankOrdersOutsideTheHorizonByHeadroomThenBySoonest(t *testing.T) {
	cands := []Candidate{
		sub("blown-early", snap(win(99, 3*time.Hour), win(99, 48*time.Hour))),
		sub("roomier-late", snap(win(85, 20*time.Hour), win(85, 48*time.Hour))),
		sub("roomier-early", snap(win(85, 4*time.Hour), win(85, 48*time.Hour))),
	}

	r := Rank(cands, opts())
	eq(t, order(r), []string{"roomier-early", "roomier-late", "blown-early"})
}

// A binding window with no reset cannot be said to return inside the horizon, so
// it falls to the far tier rather than being treated as "back immediately".
func TestRankTreatsAMissingResetAsNoRecovery(t *testing.T) {
	noReset := usage.NewWindow(pf(99), nil)
	cands := []Candidate{
		{UUID: "no-reset", Kind: identity.KindSubscription, Usage: &usage.Snapshot{FiveHour: noReset, SevenDay: noReset}},
		sub("returns", snap(win(99, 10*time.Minute), win(99, 48*time.Hour))),
	}

	r := Rank(cands, opts())
	eq(t, order(r), []string{"returns", "no-reset"})
	for _, x := range r.Order {
		if x.UUID == "no-reset" && x.HasRecovery {
			t.Error("HasRecovery = true for a window that reported no reset")
		}
	}
}

func pf(v float64) *float64 { return &v }

// ---- consume-first ---------------------------------------------------------

// Weekly quota is perishable — use it or lose it — so this ranks by the SOONEST
// weekly reset, which is the opposite direction from headroom and cannot be
// folded into the same comparator by flipping a sign.
func TestConsumeFirstPrefersTheSoonestWeeklyReset(t *testing.T) {
	cands := []Candidate{
		sub("resets-late", snap(win(5, time.Hour), win(5, 5*24*time.Hour))),
		sub("resets-soon", snap(win(60, time.Hour), win(60, 12*time.Hour))),
	}

	o := opts()
	o.Strategy = StrategyConsumeFirst
	r := Rank(cands, o)
	if r.Mode != ModeConsumeFirst {
		t.Fatalf("Mode = %v, want ModeConsumeFirst", r.Mode)
	}
	eq(t, order(r), []string{"resets-soon", "resets-late"})
}

func TestConsumeFirstIgnoresTheThresholdSituation(t *testing.T) {
	// Every account is over threshold, which would put the default strategy in
	// recovery mode. consume-first is a user choice and stays chosen.
	cands := []Candidate{
		sub("resets-late", snap(win(95, time.Hour), win(95, 5*24*time.Hour))),
		sub("resets-soon", snap(win(99, 40*time.Hour), win(99, 12*time.Hour))),
	}

	o := opts()
	o.Strategy = StrategyConsumeFirst
	r := Rank(cands, o)
	if r.Mode != ModeConsumeFirst {
		t.Fatalf("Mode = %v, want ModeConsumeFirst", r.Mode)
	}
	eq(t, order(r), []string{"resets-soon", "resets-late"})
}

func TestConsumeFirstSortsAccountsWithNoWeeklyResetLast(t *testing.T) {
	cands := []Candidate{
		{UUID: "no-weekly", Kind: identity.KindSubscription, Usage: snap(win(10, time.Hour), unread())},
		sub("weekly", snap(win(10, time.Hour), win(10, 5*24*time.Hour))),
	}

	o := opts()
	o.Strategy = StrategyConsumeFirst
	eq(t, order(Rank(cands, o)), []string{"weekly", "no-weekly"})
}

// ---- determinism -----------------------------------------------------------

// Ranking over a map without a deterministic final key gives a different winner
// each tick and the cooldown never converges.
func TestRankBreaksTiesOnUUID(t *testing.T) {
	same := func(uuid string) Candidate { return sub(uuid, snap(win(30, time.Hour), win(30, 48*time.Hour))) }

	for _, input := range [][]Candidate{
		{same("ccc"), same("aaa"), same("bbb")},
		{same("bbb"), same("ccc"), same("aaa")},
		{same("aaa"), same("bbb"), same("ccc")},
	} {
		eq(t, order(Rank(input, opts())), []string{"aaa", "bbb", "ccc"})
	}
}

func TestRankDoesNotReorderItsInput(t *testing.T) {
	cands := []Candidate{
		sub("zzz", snap(win(90, time.Hour), win(90, 48*time.Hour))),
		sub("aaa", snap(win(10, time.Hour), win(10, 48*time.Hour))),
	}

	Rank(cands, opts())
	if cands[0].UUID != "zzz" {
		t.Error("Rank() reordered the slice it was handed")
	}
}

func TestRankOnAnEmptyPool(t *testing.T) {
	r := Rank(nil, opts())
	if len(r.Order) != 0 {
		t.Errorf("Order = %v, want empty", order(r))
	}
	// An empty pool has no account that is under threshold, so there is nothing
	// left to try — which is what the credit gate needs to hear.
	if !r.AllOverThreshold {
		t.Error("AllOverThreshold = false for an empty pool; there is no subscription target left")
	}
}

// ---- the credit gate's input ------------------------------------------------

// §7.3 step 2 asks whether the SUBSCRIPTION pool is exhausted, and only credit
// accounts are ranked separately. Money is the fail-closed direction: an
// unreadable subscription account means "not exhausted", so ccdad does not start
// spending because one poll failed.
func TestSubscriptionExhausted(t *testing.T) {
	credit := func(uuid string, s *usage.Snapshot) Candidate {
		return Candidate{UUID: uuid, Kind: identity.KindCredit, Usage: s}
	}
	cases := []struct {
		name  string
		cands []Candidate
		want  bool
	}{
		{"one has room", []Candidate{sub("a", snap(win(10, time.Hour), win(10, 48*time.Hour)))}, false},
		{"all spent", []Candidate{sub("a", snap(win(99, time.Hour), win(99, 48*time.Hour)))}, true},
		{"one unreadable", []Candidate{
			sub("a", snap(win(99, time.Hour), win(99, 48*time.Hour))),
			sub("b", nil),
		}, false},
		{"no subscription accounts at all", []Candidate{credit("c", nil)}, true},
		{"credit accounts do not count", []Candidate{
			sub("a", snap(win(99, time.Hour), win(99, 48*time.Hour))),
			credit("c", snap(win(1, time.Hour), win(1, 48*time.Hour))),
		}, true},
		{"a disabled account does not hold the pool open", []Candidate{
			sub("a", snap(win(99, time.Hour), win(99, 48*time.Hour))),
			{UUID: "off", Kind: identity.KindSubscription, Disabled: true, Usage: snap(win(1, time.Hour), win(1, 48*time.Hour))},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SubscriptionExhausted(tc.cands, opts()); got != tc.want {
				t.Errorf("SubscriptionExhausted() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestModesAllHaveNames(t *testing.T) {
	for m := ModeHeadroom; m <= ModeConsumeFirst; m++ {
		if m.String() == "" || m.String() == "unknown" {
			t.Errorf("Mode(%d) has no name", m)
		}
	}
}

func credit(uuid string, s *usage.Snapshot) Candidate {
	return Candidate{UUID: uuid, Kind: identity.KindCredit, Usage: s}
}

// A credit account is metered in money and carries no plan windows, so its
// headroom is permanently unknown. Ranking it on that axis files it in the "we
// have no idea" tier -- ahead of every account known to be spent -- and makes
// the engine's best candidate the one that costs money, which inverts §7.3.
func TestRankKeepsCreditAccountsOffTheHeadroomAxis(t *testing.T) {
	cands := []Candidate{
		sub("soon", snap(win(99, 8*time.Minute), win(99, 48*time.Hour))),
		sub("later", snap(win(85, 40*time.Hour), win(85, 40*time.Hour))),
		credit("money", nil),
	}

	r := Rank(cands, opts())
	eq(t, order(r), []string{"soon", "later"})
	if len(r.Credit) != 1 || r.Credit[0].UUID != "money" {
		t.Errorf("Credit = %v, want the one credit account", r.Credit)
	}
	// And with the credit account out of the fold, §7.1's second situation is
	// reachable again: both subscription accounts really are over threshold.
	if !r.AllOverThreshold {
		t.Error("AllOverThreshold = false; a credit account's unknown headroom must not pin it")
	}
	if r.Mode != ModeRecovery {
		t.Errorf("Mode = %v, want ModeRecovery — a registered credit account must not make recovery mode unreachable", r.Mode)
	}
}

func TestRankOrdersTheCreditPoolDeterministically(t *testing.T) {
	for _, input := range [][]Candidate{
		{credit("ccc", nil), credit("aaa", nil), credit("bbb", nil)},
		{credit("bbb", nil), credit("ccc", nil), credit("aaa", nil)},
	} {
		r := Rank(input, opts())
		got := []string{}
		for _, x := range r.Credit {
			got = append(got, x.UUID)
		}
		eq(t, got, []string{"aaa", "bbb", "ccc"})
	}
}

func TestRankStillExcludesADisabledCreditAccount(t *testing.T) {
	cands := []Candidate{{UUID: "off", Kind: identity.KindCredit, Disabled: true}}
	r := Rank(cands, opts())
	if len(r.Credit) != 0 || len(r.Order) != 0 {
		t.Errorf("a disabled credit account was ranked: order=%v credit=%v", order(r), r.Credit)
	}
}

// The two knobs the engine is configurable on have to be read from Options, or
// task 47's `ccdad config` will have no effect on any decision.
func TestRankReadsTheConfiguredThreshold(t *testing.T) {
	// 70% used: over a threshold of 60, under the default of 80.
	cands := []Candidate{sub("a", snap(win(70, time.Hour), win(70, 48*time.Hour)))}

	o := opts()
	o.Threshold = 60
	if r := Rank(cands, o); !r.AllOverThreshold || r.Mode != ModeRecovery {
		t.Errorf("threshold 60: AllOverThreshold = %v, Mode = %v; want true/recovery", r.AllOverThreshold, r.Mode)
	}
	o.Threshold = 80
	if r := Rank(cands, o); r.AllOverThreshold || r.Mode != ModeHeadroom {
		t.Errorf("threshold 80: AllOverThreshold = %v, Mode = %v; want false/headroom", r.AllOverThreshold, r.Mode)
	}
}

func TestRankReadsTheConfiguredHorizon(t *testing.T) {
	// Back in 30 minutes: inside the default hour, outside a 15-minute horizon.
	cands := []Candidate{
		sub("aaa-roomier-later", snap(win(85, 30*time.Minute), win(85, 48*time.Hour))),
		sub("zzz-blown-sooner", snap(win(99, 5*time.Minute), win(99, 48*time.Hour))),
	}

	o := opts()
	o.Horizon = 15 * time.Minute
	// Only zzz is inside a 15-minute horizon, so it wins its tier outright; aaa
	// falls to the far tier where headroom leads.
	r := Rank(cands, o)
	eq(t, order(r), []string{"zzz-blown-sooner", "aaa-roomier-later"})
	for _, x := range r.Order {
		if x.UUID == "aaa-roomier-later" && x.ReturnsInsideHorizon {
			t.Error("a 30-minute recovery counted as inside a 15-minute horizon")
		}
	}
}

// The zero value of Options must not be the one that starts spending money.
func TestRankDefaultsAnOmittedThresholdRatherThanReadingItAsZero(t *testing.T) {
	cands := []Candidate{sub("a", snap(win(3, time.Hour), win(3, 48*time.Hour)))}
	bare := Options{Now: now}

	if r := Rank(cands, bare); r.AllOverThreshold {
		t.Error("AllOverThreshold = true for an account 3% used; a zero Threshold must not mean 'spent at any usage'")
	}
	if SubscriptionExhausted(cands, bare) {
		t.Error("SubscriptionExhausted() = true for an account 3% used — that is the input that opens the credit gate")
	}
}

func TestRankDefaultsAnOmittedHorizon(t *testing.T) {
	cands := []Candidate{sub("a", snap(win(99, 30*time.Minute), win(99, 48*time.Hour)))}

	r := Rank(cands, Options{Now: now})
	if len(r.Order) != 1 || !r.Order[0].ReturnsInsideHorizon {
		t.Error("a 30-minute recovery fell outside the defaulted one-hour horizon")
	}
}

// ---- the credit pool's own order -------------------------------------------
//
// Result.Credit was ordered by uuid — deterministic and arbitrary — because the
// natural key needs a ceiling and the ceiling had no source until
// ~/.ccdad/config.toml existed. It has one now.

// creditRoom builds a credit account with limit and used in WIRE amounts
// (cents), which is what usage.ExtraUsage stores.
func creditRoom(uuid string, limitCents, usedCents float64) Candidate {
	return credit(uuid, &usage.Snapshot{ExtraUsage: usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD",
		MonthlyLimit: &limitCents, UsedCredits: &usedCents,
	})})
}

func creditOrder(r Result) []string {
	out := make([]string, 0, len(r.Credit))
	for _, x := range r.Credit {
		out = append(out, x.UUID)
	}
	return out
}

// The uuids run OPPOSITE to the answer, so the old uuid ordering cannot
// reproduce a passing result.
func TestTheCreditPoolIsOrderedByMostArmedRoom(t *testing.T) {
	cands := []Candidate{
		creditRoom("aaa-nearly-spent", 10000, 8000), // $100 cap, $80 spent -> $10 armed
		creditRoom("zzz-untouched", 10000, 0),       // $100 cap, nothing spent -> $90 armed
	}

	o := opts()
	o.MaxAutoSpend = 200
	r := Rank(cands, o)

	eq(t, creditOrder(r), []string{"zzz-untouched", "aaa-nearly-spent"})
	if !r.Credit[0].HasCreditRoom || r.Credit[0].CreditRoom != 90 {
		t.Errorf("CreditRoom = %v (known %v), want 90", r.Credit[0].CreditRoom, r.Credit[0].HasCreditRoom)
	}
}

// Identical room must not reorder between two ticks, or the cooldown never
// converges: the engine would switch, find a new "best", and switch back.
func TestEqualCreditRoomFallsBackToTheUuidTieBreak(t *testing.T) {
	o := opts()
	o.MaxAutoSpend = 200
	for _, input := range [][]Candidate{
		{creditRoom("ccc", 10000, 100), creditRoom("aaa", 10000, 100), creditRoom("bbb", 10000, 100)},
		{creditRoom("bbb", 10000, 100), creditRoom("ccc", 10000, 100), creditRoom("aaa", 10000, 100)},
	} {
		eq(t, creditOrder(Rank(input, o)), []string{"aaa", "bbb", "ccc"})
	}
}

// An account the gate would refuse has no room to compare, so it sorts behind
// every account that has one — whatever its uuid.
func TestACreditAccountWithNoArmedRoomSortsBehindOneThatHasSome(t *testing.T) {
	o := opts()
	o.MaxAutoSpend = 200
	// Both input orders, because a comparator that answers true in BOTH
	// directions for the same pair can still produce the right answer from one
	// of them by luck — which is exactly what dropping the no-room tier does.
	for _, cands := range [][]Candidate{
		{
			creditRoom("aaa-spent", 10000, 10000),       // nothing armed left
			credit("bbb-unreadable", &usage.Snapshot{}), // never read
			creditRoom("zzz-has-room", 10000, 0),        // $90 armed
		},
		{
			creditRoom("zzz-has-room", 10000, 0),
			creditRoom("aaa-spent", 10000, 10000),
			credit("bbb-unreadable", &usage.Snapshot{}),
		},
	} {
		eq(t, creditOrder(Rank(cands, o)), []string{"zzz-has-room", "aaa-spent", "bbb-unreadable"})
	}
}

// With the default ceiling nothing is armed at all, so the pool keeps the
// deterministic uuid order it had before this key existed.
func TestWithNoCeilingTheCreditPoolStaysInUuidOrder(t *testing.T) {
	cands := []Candidate{creditRoom("zzz", 10000, 0), creditRoom("aaa", 10000, 5000)}

	r := Rank(cands, opts())
	eq(t, creditOrder(r), []string{"aaa", "zzz"})
	if r.Credit[0].HasCreditRoom {
		t.Error("HasCreditRoom = true with max_auto_spend at its default of 0")
	}
}

// The ordering must agree with the gate that later approves the account, or the
// engine would walk to a first choice the gate then refuses while a usable one
// waits behind it. The account's OWN limit binds here, not the ceiling.
func TestTheCreditOrderUsesTheSmallerOfTheAccountLimitAndTheCeiling(t *testing.T) {
	cands := []Candidate{
		creditRoom("aaa-big-limit", 100000, 0), // $1000 cap, but the $50 ceiling binds -> $45
		creditRoom("zzz-small-limit", 6000, 0), // $60 cap binds under the $50 ceiling? no: min(60,50)=50 -> $45
	}
	o := opts()
	o.MaxAutoSpend = 50
	r := Rank(cands, o)
	// Both cap at the ceiling, so both have the same room and the tie-break
	// decides. What matters is that neither reads as $900 of room.
	for _, x := range r.Credit {
		if x.CreditRoom != 45 {
			t.Errorf("%s: CreditRoom = %v, want 45 — the cap is min(account limit, ceiling)", x.UUID, x.CreditRoom)
		}
	}
	eq(t, creditOrder(r), []string{"aaa-big-limit", "zzz-small-limit"})
}
