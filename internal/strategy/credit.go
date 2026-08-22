// Package strategy decides which account the engine should be on.
package strategy

import (
	"math"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Spec §7.3, the credit gate.
//
// Subscription quota is already paid for; credits cost money. This is the only
// place in ccdad where a bug spends the user's, so every branch here fails
// closed: an unreadable figure refuses rather than reading as zero, and an
// unbounded ceiling refuses rather than being honoured.

// spendArmFraction arms a switch at 90% of the allowed cap, so a switch never
// lands exactly on the ceiling.
const spendArmFraction = 0.90

// SpendInfo is the credit axis, in the shape §7.3's function takes. Limit and
// Used are pointers because the wire declares both nullable and the difference
// between "not reported" and "zero" is the difference between refusing and
// spending.
type SpendInfo struct {
	// Enabled is the account's own overage switch — one of the two independent
	// opt-ins. The other is the configured ceiling.
	Enabled bool
	// Limit is the account's own monthly cap. Nil means the account sets none,
	// so the configured ceiling is the only bound.
	Limit *float64
	// Used is what has already been spent. Nil means it could not be read, and
	// that refuses.
	Used *float64
}

// CreditRoom is §7.3's function, unchanged except that the spec's local `cap` is
// named `capacity` here so it does not shadow the builtin.
//
// The IsInf and IsNaN checks are load-bearing, not decoration. `max_auto_spend =
// inf` is valid TOML, and an infinite ceiling with no account cap yields
// unlimited unattended spending; NaN slips a plain `<= 0` because every NaN
// comparison is false.
func CreditRoom(s SpendInfo, ceiling float64) (float64, bool) {
	if !s.Enabled || math.IsInf(ceiling, 0) || math.IsNaN(ceiling) || ceiling <= 0 {
		return 0, false
	}
	capacity := ceiling
	if s.Limit != nil {
		capacity = math.Min(*s.Limit, ceiling)
	}
	if s.Used == nil {
		return 0, false // wire drift must not hand back the full cap
	}
	// The float64 conversion is load-bearing, not decoration. Go permits a
	// compiler to fuse `a*b - c` into a single FMA -- possibly ACROSS
	// statements, so an intermediate variable does not stop it -- and arm64
	// does. Fused, the exact product 0.90*100 (90.000000000000002220446…) is
	// never rounded to 90 before the subtraction, so $90 spent against a $100
	// cap answers 2.22e-15 of room, and `room > 0` is TRUE. That is not a
	// display artefact: it is the credit gate reporting room on the one axis
	// in ccdad where a wrong answer spends the user's money, and darwin/arm64
	// and linux/arm64 are two of the six shipped targets.
	//
	// An explicit conversion is the one thing the spec says forbids the
	// fusion, which makes all six targets agree. Measured: green on amd64,
	// red on macos-latest (arm64), where CreditRoom(limit 100, used 80) came
	// back as 10.000000000000002.
	room := float64(spendArmFraction*capacity) - *s.Used
	// A NaN anywhere in the account's own figures reaches here as a NaN room,
	// and `room > 0` is false for it — which is the fail-closed answer.
	return room, room > 0
}

// CreditReason says why the gate answered as it did. It reaches a user-facing
// notification, so every value has a name.
type CreditReason uint8

const (
	// CreditAllowed: there is armed room and both opt-ins are in place.
	CreditAllowed CreditReason = iota
	// CreditSubscriptionNotExhausted: it is not the credit pool's turn. Steps 1
	// and 2 of the gate order — the credit pool is consulted only once the
	// subscription pool is EXHAUSTED, not merely once it has failed hysteresis.
	CreditSubscriptionNotExhausted
	// CreditNotOptedIn: max_auto_spend is 0, which is the default.
	CreditNotOptedIn
	// CreditAccountDisabled: the account's own overage switch is off.
	CreditAccountDisabled
	// CreditAccountBlocked: the organization or seat refused overage — a spend
	// cap reached, credits out. Different from disabled, and worth saying so.
	CreditAccountBlocked
	// CreditSpendUnreadable: the figures needed to price a switch are missing.
	CreditSpendUnreadable
	// CreditNoRoom: the armed cap is spent.
	CreditNoRoom
	// CreditCeilingInvalid: the configured ceiling is infinite, NaN or negative.
	CreditCeilingInvalid
)

func (r CreditReason) String() string {
	switch r {
	case CreditAllowed:
		return "allowed"
	case CreditSubscriptionNotExhausted:
		return "subscription quota remains"
	case CreditNotOptedIn:
		return "max_auto_spend is 0"
	case CreditAccountDisabled:
		return "the account has extra usage switched off"
	case CreditAccountBlocked:
		return "the organization refused extra usage"
	case CreditSpendUnreadable:
		return "the account's spend could not be read"
	case CreditNoRoom:
		return "the allowed spend is used up"
	case CreditCeilingInvalid:
		return "max_auto_spend is not a usable amount"
	}
	return "unknown"
}

// Decision is the gate's answer.
type Decision struct {
	// Allow is whether the engine may switch to this credit account.
	Allow bool
	// Reason is why.
	Reason CreditReason
	// Blocked marks a refusal the user should act on: exit 4 and a
	// notification, never a silent exit 3. cswap conflates these two and a
	// money-blocked engine becomes invisible to cron (§9.3).
	Blocked bool
	// Room is the armed spend left, when there is any.
	Room float64
	// DisabledReason is the organization's own word for its refusal, kept
	// verbatim: it is the difference between "turn overage on" and "your org
	// hit its spend cap".
	DisabledReason string
}

// CreditGate applies §7.3's four steps to one credit account.
//
// subscriptionExhausted is steps 1 and 2, supplied by the ranking pass: it must
// mean the subscription pool has no viable target left, not that the best
// subscription candidate merely failed hysteresis.
//
// ceiling is passed in rather than read from config so this lands without
// waiting on `ccdad config`; it is max_auto_spend, and 0 is its default.
func CreditGate(e usage.ExtraUsage, ceiling float64, subscriptionExhausted bool) Decision {
	d := Decision{DisabledReason: e.DisabledReason}

	if !subscriptionExhausted {
		// Not actionable: there is still paid-for quota to use, so there is
		// nothing here for the user to fix.
		d.Reason = CreditSubscriptionNotExhausted
		return d
	}

	// Every refusal from here on is one the user can do something about, so
	// they all carry Blocked.
	d.Blocked = true

	switch e.State {
	case usage.ExtraUsageDisabled:
		d.Reason = CreditAccountDisabled
		return d
	case usage.ExtraUsageBlocked:
		d.Reason = CreditAccountBlocked
		return d
	case usage.ExtraUsageUnknown:
		// An account whose credit axis could not be read is one ccdad cannot
		// price. Refuse, and say which of the two it is.
		d.Reason = CreditSpendUnreadable
		return d
	}

	// The ceiling is checked before CreditRoom folds every refusal into one
	// boolean, so "you never opted in" and "your configured limit is not a
	// number" reach the notification as different sentences.
	switch {
	case ceiling == 0:
		d.Reason = CreditNotOptedIn
		return d
	case math.IsInf(ceiling, 0) || math.IsNaN(ceiling) || ceiling < 0:
		d.Reason = CreditCeilingInvalid
		return d
	}

	limit, hasLimit := e.MonthlyLimit()
	used, hasUsed := e.UsedCredits()
	s := SpendInfo{Enabled: true}
	if hasLimit {
		s.Limit = &limit
	}
	if hasUsed {
		s.Used = &used
	}
	if !hasUsed {
		d.Reason = CreditSpendUnreadable
		return d
	}

	room, ok := CreditRoom(s, ceiling)
	if !ok {
		d.Reason = CreditNoRoom
		return d
	}
	return Decision{Allow: true, Reason: CreditAllowed, Room: room, DisabledReason: e.DisabledReason}
}
