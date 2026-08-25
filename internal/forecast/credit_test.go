package forecast

import (
	"math"
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
			got := creditFleet(c.in, now)
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
	}, now)
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
	}, now)
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
	}, now)
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
	}, now)
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
		}, now)
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
