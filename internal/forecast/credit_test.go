package forecast

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/history"
)

// f is a pointer to a float literal, which is how the tri-state limit is
// written in these fixtures: nil is an account that reported no cap at all, and
// that is not a cap of zero.
func f(v float64) *float64 { return &v }

// creditSample builds one sample carrying a credit reading and no windows.
//
// The windows are absent deliberately. The credit rule shares none of the
// window rule's segmenting -- a credit reading has no reset to segment on -- so
// a fixture that also carried a window would leave a test green if the
// implementation reached for the wrong one.
func creditSample(at time.Time, used float64) history.Sample {
	return history.Sample{At: at, Credit: &history.Credit{Used: used, Currency: "USD"}}
}

// Each of these rows is a way to produce a confident, concrete, wrong date, and
// each was found by working the arithmetic rather than by running it.
func TestCreditRefusesRatherThanGuessing(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		in   []creditInput
	}{
		// Its pool has no bottom, so leaving it out understates the fleet's
		// balance and dates a fleet that cannot run out.
		{"an unlimited account is spending", []creditInput{
			{used: 10, limit: nil, rate: 2, currency: "USD"},
			{used: 5, limit: f(100), rate: 1, currency: "USD"},
		}},
		// A two-decimal currency's figures were divided by 100 on the way in
		// and a zero-decimal currency's were not, so adding them adds two
		// different quantities.
		{"two currencies", []creditInput{
			{used: 10, limit: f(100), rate: 2, currency: "USD"},
			{used: 10, limit: f(100), rate: 2, currency: "KRW"},
		}},
		// A measured zero is a rate, not an absence, so nothing upstream
		// excludes it -- and balance/0 is +Inf, which becomes a date rather
		// than an error.
		{"the summed rate is zero", []creditInput{
			{used: 10, limit: f(100), rate: 0, currency: "USD"},
		}},
		{"nothing is enabled", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := creditFleet(c.in, creditBasis{}, now)
			if got.Known {
				t.Fatalf("Known = true, DryAt = %v -- this case must refuse", got.DryAt)
			}
		})
	}
}

// Without this test every refusal above passes on an implementation that
// answers nothing at all, so this is the one that says a figure exists.
func TestCreditDividesOneFleetBalanceByOneFleetRate(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	got := creditFleet([]creditInput{
		{used: 40, limit: f(100), rate: 1.5, currency: "USD"},
		{used: 10, limit: f(50), rate: 0.5, currency: "USD"},
	}, creditBasis{}, now)
	if !got.Known {
		t.Fatal("Known = false, want a figure")
	}
	// 60 left plus 40 left is 100, spent at 1.5 plus 0.5 an hour: 50 hours.
	if want := now.Add(50 * time.Hour); !got.DryAt.Equal(want) {
		t.Errorf("DryAt = %v, want %v", got.DryAt, want)
	}
	if got.SpendPerHour != 2 {
		t.Errorf("SpendPerHour = %v, want 2", got.SpendPerHour)
	}
	if got.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", got.Currency)
	}
}

// The refusal on an uncapped account is qualified by "and it is spending", and
// this pins the qualification. An account with no cap that spends nothing can
// make nothing run out, so it is skipped; refusing on it would blank the credit
// figure for every fleet that has one idle uncapped seat in it.
func TestAnUncappedAccountThatSpendsNothingIsSkippedNotRefused(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	got := creditFleet([]creditInput{
		{used: 7, limit: nil, rate: 0, currency: "USD"},
		{used: 0, limit: f(20), rate: 2, currency: "USD"},
	}, creditBasis{}, now)
	if !got.Known {
		t.Fatal("Known = false, want a figure from the capped account alone")
	}
	if want := now.Add(10 * time.Hour); !got.DryAt.Equal(want) {
		t.Errorf("DryAt = %v, want %v", got.DryAt, want)
	}
	if got.SpendPerHour != 2 {
		t.Errorf("SpendPerHour = %v, want 2", got.SpendPerHour)
	}
}

// An account whose used has passed its own cap cannot spend the overshoot back,
// so its spendable balance is zero and not a negative one. Summed without a
// per-account floor, one account's overshoot pays for another account's real
// money and the fleet's dry moment lands in the past.
func TestAnAccountPastItsOwnCapContributesNoBalanceRatherThanADebt(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	got := creditFleet([]creditInput{
		{used: 130, limit: f(100), rate: 1, currency: "USD"},
		{used: 0, limit: f(10), rate: 1, currency: "USD"},
	}, creditBasis{}, now)
	if !got.Known {
		t.Fatal("Known = false, want a figure")
	}
	if want := now.Add(5 * time.Hour); !got.DryAt.Equal(want) {
		t.Errorf("DryAt = %v, want %v", got.DryAt, want)
	}
}

// 1e9 major units drained at a nanounit an hour is 1e18 hours, and 1e18 hours
// is not a time.Duration. Measured on linux/amd64: converting that float64
// yields -9223372036854775808 ns, so now.Add of it dates the fleet's credits to
// 1734 -- the concrete wrong date this whole rule exists to refuse.
func TestACreditRunwayTooLongToStateRefuses(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	got := creditFleet([]creditInput{
		{used: 0, limit: f(1e9), rate: 1e-9, currency: "USD"},
	}, creditBasis{}, now)
	if got.Known {
		t.Fatalf("Known = true, DryAt = %v -- an interval this long has no date", got.DryAt)
	}
}

// A NaN is what the fail-closed test on the summed rate cannot see, because
// every comparison against a NaN is false and `rate <= 0` is a comparison. It
// then converts to the same time.Duration an out-of-range float does, so a
// fleet whose spend rate could not be computed would date its credits to 1734
// rather than declining to date them.
func TestANonFiniteSpendRateRefuses(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for _, rate := range []float64{math.NaN(), math.Inf(-1)} {
		got := creditFleet([]creditInput{
			{used: 0, limit: f(100), rate: rate, currency: "USD"},
		}, creditBasis{}, now)
		if got.Known {
			t.Errorf("rate %v: Known = true, DryAt = %v -- this rate is not a measurement", rate, got.DryAt)
		}
	}
}

// The billing rollover is a DROP in used. There is no reset to segment on, and
// a sum that let the drop through would report the fleet earning money back.
func TestCreditSegmentsOnADropInUsed(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	s := []history.Sample{
		creditSample(base, 90),
		creditSample(base.Add(time.Hour), 95),
		creditSample(base.Add(2*time.Hour), 3), // billing rollover
		creditSample(base.Add(3*time.Hour), 8),
	}
	filled, _, _, ok := creditSpend(s, base, base.Add(4*time.Hour))
	if !ok {
		t.Fatal("ok = false")
	}
	if filled != 10 { // 5 before, 5 after; the drop of 92 costs nothing
		t.Fatalf("spend = %v, want 10", filled)
	}
}

// Below the gates there is no rate, which is not a rate of zero: an account
// that reports unmeasured is absent from every sum, while one that reports zero
// is summed as having spent nothing.
func TestCreditSpendRefusesBelowTheGatesRatherThanReportingZero(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	to := base.Add(4 * time.Hour)
	for _, c := range []struct {
		name string
		in   []history.Sample
	}{
		{"one difference cannot be told from a rounding step", []history.Sample{
			creditSample(base, 10),
			creditSample(base.Add(time.Hour), 20),
		}},
		{"three readings arriving in one burst", []history.Sample{
			creditSample(base, 10),
			creditSample(base.Add(time.Minute), 12),
			creditSample(base.Add(3*time.Minute), 14),
		}},
		// A stale account is unmeasured, not slow: rating one whose newest
		// reading no longer carries a credit would freeze a spend rate from
		// evidence that stopped arriving.
		{"the newest sample reports no credit at all", []history.Sample{
			creditSample(base, 10),
			creditSample(base.Add(time.Hour), 12),
			creditSample(base.Add(2*time.Hour), 14),
			{At: base.Add(3 * time.Hour)},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if spent, _, _, ok := creditSpend(c.in, base, to); ok {
				t.Fatalf("ok = true, spend = %v -- this case has no rate", spent)
			}
		})
	}
}

// A gap in the credit readings is a gap in the evidence, not a break in the
// chain: an account still spent across it, and pairing over the gap counts that
// spend once. What must never happen is carrying the last reading forward into
// the gap, which would count the spend as though it had not happened.
func TestASampleWithoutCreditDoesNotBreakTheChain(t *testing.T) {
	base := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	s := []history.Sample{
		creditSample(base, 10),
		{At: base.Add(30 * time.Minute)},
		creditSample(base.Add(time.Hour), 16),
		creditSample(base.Add(2*time.Hour), 19),
	}
	filled, cover, samples, ok := creditSpend(s, base, base.Add(4*time.Hour))
	if !ok {
		t.Fatal("ok = false")
	}
	if filled != 9 {
		t.Errorf("spend = %v, want 9", filled)
	}
	if samples != 3 {
		t.Errorf("samples = %v, want 3", samples)
	}
	if cover != 2*time.Hour {
		t.Errorf("cover = %v, want 2h", cover)
	}
}

// liveRolloverSeries is testdata/credit-rollover-2026-09-01.json: the
// ~/.ccdad/history.json of the claude_enterprise seat on aaron-internal-server,
// copied 2026-09-01 at 14:18 KST while its daemon was still appending to it.
//
// It is the first series this repository has ever held in which used_credits
// MOVES. Every earlier reading from that account was pegged at its cap -- used
// == limit == 466 USD, utilization 100, across samples nine minutes apart -- and
// a flat series measures no rate, so creditSpend, SpendPerHour and DryAt had
// never been entered by anything except a fixture written to enter them. Fifty
// one samples carry four distinct balances:
//
//	06:22:17 KST  used = 466     the cap, where the account had sat for days
//	09:04:36 KST  used = 0       the billing month rolled over
//	12:55:28 KST  used = 0.01    spending resumed
//	13:14:22 KST  used = 0.02
//
// The fall at 09:04 is the segment boundary creditSpend implements and the
// rises after it are the rate, and having both in one file is what makes this
// series worth keeping rather than a longer flat one.
//
// It is verbatim except for the account UUID, which is a KEY in this document
// and is replaced by a well-formed placeholder. No timestamp, balance, limit or
// currency is edited and no sample is elided -- including the two pairs of
// samples 0.6 s apart, at 11:15:48 and 12:13:44, which are what a scheduled
// poll and a forced refresh landing together look like. They are left in
// because the arithmetic has to survive them, not because they are tidy.
func liveRolloverSeries(t *testing.T) []history.Sample {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "credit-rollover-2026-09-01.json"))
	if err != nil {
		t.Fatalf("reading the recorded series: %v", err)
	}
	var file struct {
		Accounts map[string]history.Account `json:"accounts"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatalf("parsing the recorded series: %v", err)
	}
	if len(file.Accounts) != 1 {
		t.Fatalf("the recorded series holds %d accounts, want exactly 1", len(file.Accounts))
	}
	for _, a := range file.Accounts {
		return a.Samples
	}
	return nil
}

// kst is the zone the recorded samples carry. The series is read in it rather
// than in UTC so that the moments named in these tests are the moments a reader
// comparing them against the file sees written there.
var kst = time.FixedZone("KST", 9*60*60)

// liveEnd is the last reading in the recorded series, and the moment these
// tests stand at unless they say otherwise.
var liveEnd = time.Date(2026, 9, 1, 14, 14, 6, 93813287, kst)

// TestTheLiveSeriesMeasuresASpendRate is the test this item was filed to make
// possible: the first time creditSpend has run against a balance that moves.
//
// What it pins is the UNIT. The rate is the recorded major-unit balances over
// the hours between the readings that carry them -- two cents over the trailing
// window, not two hundred over it and not two cents per sample -- which is the
// claim CreditFleet.SpendPerHour's contract makes and the one no flat series
// could ever have checked.
func TestTheLiveSeriesMeasuresASpendRate(t *testing.T) {
	series := liveRolloverSeries(t)
	now := liveEnd
	from := now.Add(-history.MeasuredSpan)

	spent, cover, samples, ok := creditSpend(series, from, now)
	if !ok {
		t.Fatalf("creditSpend refused the live series: spent=%v cover=%v samples=%d", spent, cover, samples)
	}
	// 0.01 and 0.02 are the two rises the file records, and doubling a float64
	// is exact, so this sum has no representation error to tolerate.
	if spent != 0.02 {
		t.Errorf("spent = %v, want 0.02 -- the two rises the trailing window contains", spent)
	}
	if samples < minSamples {
		t.Errorf("samples = %d, want at least %d", samples, minSamples)
	}
	if cover < minCover {
		t.Errorf("cover = %v, want at least %v", cover, minCover)
	}

	// The unit, stated as an identity rather than as a decimal: a rate in major
	// units per hour, multiplied by the hours it was measured over, is the
	// money that was spent. A rate divided by the wrong denominator -- seconds,
	// or the sample count -- fails here by orders of magnitude.
	rate := spent / cover.Hours()
	if back := rate * cover.Hours(); math.Abs(back-spent) > 1e-12 {
		t.Errorf("rate*hours = %v, want %v -- SpendPerHour is not per hour", back, spent)
	}
	if rate <= 0 {
		t.Fatalf("rate = %v, want a positive measured rate", rate)
	}
	t.Logf("live rate: %v USD/h over %v from %d readings", rate, cover, samples)
}

// TestTheLivePairSpanningTheBillingRolloverContributesNothing stands at a
// moment whose trailing window holds the rollover itself.
//
// The clamp in creditSpend is the whole subject. used_credits fell from 466 to
// 0 when the billing month turned over, and that fall is not money coming back:
// unclamped it would report the fleet EARNING 466 USD, which is a negative
// summed rate and a dry date in the past for any fleet it is summed into.
func TestTheLivePairSpanningTheBillingRolloverContributesNothing(t *testing.T) {
	series := liveRolloverSeries(t)
	// 12:54 KST: late enough that the window still reaches back past the
	// rollover at 09:04, early enough that the first rise at 12:55 is outside
	// it. The fall is then the only difference in range.
	now := time.Date(2026, 9, 1, 12, 54, 0, 0, kst)
	from := now.Add(-history.MeasuredSpan)

	// Without this the test passes on a window that never held the rollover at
	// all, which is the way a test of a clamp goes blind.
	var sawCap, sawFloor bool
	for _, s := range series {
		if s.At.Before(from) || s.At.After(now) || s.Credit == nil {
			continue
		}
		switch s.Credit.Used {
		case 466:
			sawCap = true
		case 0:
			sawFloor = true
		}
	}
	if !sawCap || !sawFloor {
		t.Fatalf("the window [%v, %v] does not span the rollover: sawCap=%v sawFloor=%v",
			from.In(kst), now.In(kst), sawCap, sawFloor)
	}

	spent, cover, samples, ok := creditSpend(series, from, now)
	if !ok {
		t.Fatalf("creditSpend refused a window that clears its gates: cover=%v samples=%d", cover, samples)
	}
	if spent != 0 {
		t.Fatalf("spent = %v, want 0 -- a fall in used is a rollover, not money coming back", spent)
	}

	// And the surface that reads it refuses rather than reporting a fleet that
	// spends nothing: a measured zero is a rate, and left/0 is a date.
	got := creditFleet([]creditInput{{
		used: 0, limit: f(466), rate: spent / cover.Hours(), currency: "USD",
	}}, creditBasis{}, now)
	if got.Known {
		t.Errorf("creditFleet reported DryAt = %v from a window that measured no spend", got.DryAt)
	}
}

// TestTheLiveDryDateComesFromTheArithmeticAndNotFromItsGuards answers the
// question a reader asks of any date this repository prints: is this a
// measurement, or is it a bound leaking out as a number?
//
// Both guards in creditFleet are capable of shaping a date -- maxRunwayHours
// converts a quotient no Duration can hold, and the finiteness test catches the
// NaN that defeats the sign check. Neither is reached here, and the margin
// says so.
func TestTheLiveDryDateComesFromTheArithmeticAndNotFromItsGuards(t *testing.T) {
	series := liveRolloverSeries(t)
	now := liveEnd
	spent, cover, readings, ok := creditSpend(series, now.Add(-history.MeasuredSpan), now)
	if !ok {
		t.Fatal("creditSpend refused the live series")
	}

	// The balance is the account's own reading at that moment: 466 USD of cap
	// against 0.02 USD spent, both as ~/.ccdad/usage.json recorded them.
	got := creditFleet([]creditInput{{
		used: 0.02, limit: f(466), rate: spent / cover.Hours(), currency: "USD",
	}}, creditBasis{spent: spent, observed: cover, readings: readings}, now)
	if !got.Known {
		t.Fatal("creditFleet refused a fleet with a measured rate and a readable cap")
	}
	if got.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", got.Currency)
	}

	hours := got.DryAt.Sub(now).Hours()
	if hours <= 0 {
		t.Fatalf("DryAt = %v is not ahead of now = %v", got.DryAt, now)
	}
	// Neither guard fired. maxRunwayHours does not SHAPE a date -- it refuses
	// one -- so what a passing quotient shows is that the date on the other
	// side of it is the arithmetic's and not the bound's. The margin is about
	// twenty-eight fold, which is worth logging rather than asserting: it is a
	// property of how idle this account is, and a busier one would narrow it
	// without anything being wrong.
	if math.IsNaN(hours) || hours > maxRunwayHours {
		t.Fatalf("DryAt is %v hours out, at or past maxRunwayHours (%v) -- this date would have been refused",
			hours, maxRunwayHours)
	}
	// And this is the identity that says the date IS the quotient: the
	// account's spendable balance over its measured rate, in hours. A date
	// assembled any other way -- from the cap rather than the room, or from a
	// clamped rate -- misses here.
	if want := (466 - 0.02) / (spent / cover.Hours()); math.Abs(hours-want) > 1e-6 {
		t.Errorf("DryAt is %v hours out, want %v -- the date is not balance/rate", hours, want)
	}
	t.Logf("headroom under maxRunwayHours: %.1fx", maxRunwayHours/hours)
	t.Logf("live dry date: %v (in %.0f hours)", got.DryAt.In(kst), hours)

	// The basis this date needs a reader to see. Two cents is the whole of the
	// evidence and the date is a decade out; those two facts belong to each
	// other, and until this pair was carried only one of them could be printed.
	if got.Spent != spent || got.Observed != cover || got.Readings != readings {
		t.Errorf("basis = %v USD over %v from %d readings; want %v over %v from %d",
			got.Spent, got.Observed, got.Readings, spent, cover, readings)
	}
	// And it is beyond a year by a wide margin, which is what makes this the
	// row internal/view's coarse rendering exists for. If a future recording
	// put it inside a year this assertion is the one that says so rather than
	// letting the far case quietly stop being tested.
	if hours < 365*24 {
		t.Errorf("the live dry date is %v hours out, inside a year -- this series no longer exercises the far case", hours)
	}
}
