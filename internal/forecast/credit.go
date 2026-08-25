package forecast

import (
	"math"
	"time"

	"github.com/Kweiza/ccdaddy/internal/history"
)

// maxRunwayHours is the longest interval that can become a time.Duration at
// all: math.MaxInt64 nanoseconds is 2562047.79 hours, a little over 292 years.
//
// The bound has to be tested in HOURS, before the conversion, because
// converting an out-of-range float64 to time.Duration is not saturation. Go's
// conversion rules leave the result implementation-defined, and what comes back
// on the platform this was measured on -- linux/amd64 -- is
// -9223372036854775808 ns, so a balance draining at a rate near zero would not
// print "further off than can be stated", it would date the fleet's credits to
// 1734. math.Inf and math.NaN convert to the same value, which is why the
// finiteness test below cannot be skipped either.
const maxRunwayHours = float64(math.MaxInt64) / float64(time.Hour)

// CreditFleet is the paid-usage answer, and it is deliberately one date.
//
// There is no verdict here and no replenishment figure, because credits are not
// a window: the endpoint reports no renewal boundary for them, so there is
// nothing for a rate to be sustainable against and no claim of the form "this
// lasts the month" can be made from what can be seen. A balance divided by a
// spend rate, with nothing coming back, is the whole of it. The surfaces render
// the replenishment cell as the glyph for "no such quantity here" rather than
// the one for "could not be read".
//
// Known false is a refusal and never a zero. creditFleet names every way this
// figure can fail to exist, and each of them is a reason to print nothing
// rather than a reason to print a date.
type CreditFleet struct {
	// Currency is the ISO code every contributing account agreed on. Amounts in
	// two currencies do not add, so a figure exists only when they match.
	Currency string

	// SpendPerHour is the fleet's measured spend in Currency's MAJOR unit per
	// hour -- dollars an hour for USD, not cents. usage.ExtraUsage's accessors
	// have already done that conversion; nothing here divides by 100.
	SpendPerHour float64

	// DryAt is when the summed balance reaches zero at SpendPerHour.
	DryAt time.Time

	Known bool
}

// creditSpend measures one account's paid spend over the samples in [from, to].
//
// It cannot reuse windowRate. That function segments a series on the window's
// rollover, detected from a drop in the percentage or a reset that moved
// forward by half a window; a credit reading has neither. What it has is
// used_credits, a monthly meter whose companion field is monthly_limit, so it
// DROPS at the billing rollover. The drop is the segment boundary, and the
// clamp below is what implements it: a fall in used is a rollover or a
// correction, and neither is money coming back, so the pair spanning it
// contributes nothing and the rise after it is measured from the new balance
// rather than from the old one. Letting the fall through unclamped would report
// the fleet earning.
//
// spent is that spend in the currency's major unit. cover is the span this
// account's own first and last credit readings reach across, which is not the
// requested range and not any other account's span. samples is how many
// readings carried a credit.
//
// ok false means there is NO rate, which is not a rate of zero: an account that
// fails a gate is unmeasured, absent from every sum, and never counted as
// spending nothing. spent is meaningful only when ok is true.
//
// The series must be oldest first, which is what history.Series returns. A
// sample carrying no credit is skipped rather than treated as a break in the
// chain -- an account still spent across a gap in the readings, and pairing
// across it counts that spend once -- but the last reading is never carried
// forward into the gap, because nothing read is not nothing spent.
func creditSpend(series []history.Sample, from, to time.Time) (spent float64, cover time.Duration, samples int, ok bool) {
	var (
		first, last   time.Time
		prev          float64
		havePrev      bool
		haveNewest    bool
		newestCarries bool
	)
	for _, s := range series {
		if s.At.Before(from) || s.At.After(to) {
			continue
		}
		// The staleness gate asks about the newest SAMPLE, not the newest one
		// carrying a credit, so it is recorded on every pass through the range
		// and read after the loop.
		haveNewest, newestCarries = true, s.Credit != nil
		if s.Credit == nil {
			continue
		}
		u := s.Credit.Used
		if havePrev {
			if d := u - prev; d > 0 {
				spent += d
			}
		}
		prev, havePrev = u, true
		if samples == 0 {
			first = s.At
		}
		last = s.At
		samples++
	}

	cover = last.Sub(first)
	switch {
	case !haveNewest || !newestCarries:
		// A stale account is unmeasured, not slow. Rating an account whose
		// newest reading no longer reports a credit would freeze a spend rate
		// from evidence that stopped arriving.
		return 0, cover, samples, false
	case samples < minSamples, cover < minCover:
		return 0, cover, samples, false
	}
	return spent, cover, samples, true
}

// creditInput is one account's credit position as the fleet figure sees it.
//
// A value exists only for an account whose used balance was readable and whose
// spend rate was MEASURED -- creditSpend returned ok. Both are preconditions of
// the slice rather than fields on it, because a rate of zero here has to mean
// "polled, and spent nothing", which is summed. An unmeasured account that
// reached this slice would be indistinguishable from that and would quietly
// join the sum as a zero.
type creditInput struct {
	// used is money already spent, in currency's major unit.
	used float64

	// limit is the account's own monthly cap in the same unit, and nil means
	// UNLIMITED. It is never a cap of zero: that is the opposite verdict, and
	// creditFleet reads the two differently.
	limit *float64

	// rate is this account's measured spend, major units per hour.
	rate float64

	// currency must be usage.ExtraUsage.CurrencyCode(), which is normalized and
	// never empty. It is compared for equality across the fleet because the
	// major-unit conversion is currency-dependent -- it divides by 100 for a
	// two-decimal currency and not at all for JPY, KRW and VND -- so summing
	// across two of them sums two different quantities.
	currency string
}

// creditFleet is the whole credit answer: the summed balance over the summed
// spend rate, or nothing.
//
// The sum runs only over accounts with a readable cap, a readable used balance
// and a measured rate, and only when those accounts report one currency. Four
// separate ways of getting it wrong are refused below, and each of them would
// otherwise print a concrete date that a reader has no way to distinguish from
// a measured one.
func creditFleet(in []creditInput, now time.Time) CreditFleet {
	var (
		currency string
		left     float64
		rate     float64
		have     bool
	)
	for _, c := range in {
		if c.limit == nil {
			// An uncapped account that is SPENDING refuses the whole figure.
			// Its pool has no bottom, so dropping it would leave the fleet's
			// balance understated and produce a dry date for a fleet that
			// cannot run out -- worse than declining, because a date carries no
			// mark saying which accounts it covers.
			//
			// An uncapped account spending nothing is merely skipped. It
			// contributes no balance and no rate and can make nothing run out,
			// and refusing on it would blank the credit figure for every fleet
			// carrying one idle uncapped seat.
			if c.rate != 0 {
				return CreditFleet{}
			}
			continue
		}
		if have && c.currency != currency {
			return CreditFleet{}
		}
		currency, have = c.currency, true
		// The floor is per account, not around the sum. An account whose used
		// has passed its own cap cannot spend the overshoot back -- the cap
		// stops it -- so its spendable balance is zero; letting the negative
		// through would have one account's overshoot pay for another account's
		// real money and could date the fleet's credits in the past.
		if room := *c.limit - c.used; room > 0 {
			left += room
		}
		rate += c.rate
	}

	// A measured zero is a rate, not an absence, so nothing upstream filters it
	// out -- and left/0 is +Inf, which becomes a concrete wrong date rather
	// than an error. Refusing here rather than after the division is what keeps
	// a non-finite quotient from ever forming.
	//
	// It is deliberately DOUBLED with the bound below, and the doubling was
	// measured: with this test cut to `!have`, a fleet whose summed rate is
	// zero is still refused, because the +Inf quotient it then produces exceeds
	// that bound. The two guards separate only on a NEGATIVE summed rate, which
	// the bound lets through as a date in the past. So neither is redundant and
	// neither should be deleted as unreachable -- this one carries the sign,
	// the one below carries the magnitude.
	if !have || rate <= 0 {
		return CreditFleet{}
	}
	// One bound on the quotient, because the quotient is the only value the
	// conversion below sees. NaN is named explicitly because it is the value
	// that defeats the guard above: every comparison against a NaN is false, so
	// a NaN rate passes `rate <= 0` and would reach a conversion that does not
	// define it. An infinite balance divided by a finite rate exceeds the bound
	// and is refused by the same line.
	hours := left / rate
	if math.IsNaN(hours) || hours > maxRunwayHours {
		return CreditFleet{}
	}
	return CreditFleet{
		Currency:     currency,
		SpendPerHour: rate,
		DryAt:        now.Add(time.Duration(hours * float64(time.Hour))),
		Known:        true,
	}
}
