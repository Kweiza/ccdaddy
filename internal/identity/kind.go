package identity

import "strings"

// Kind is how an account pays for inference, which decides how the auto-switch
// engine ranks it.
type Kind uint8

const (
	// KindSubscription is metered by the five-hour and seven-day windows.
	KindSubscription Kind = iota
	// KindCredit is metered in money. The engine treats these as a last resort:
	// subscription quota is already paid for, credits are not.
	KindCredit
	// KindAPIKey has no quota concept, so the usage-aware strategies never
	// consider it.
	KindAPIKey
)

func (k Kind) String() string {
	switch k {
	case KindCredit:
		return "credit"
	case KindAPIKey:
		return "api-key"
	default:
		return "subscription"
	}
}

// ParseKind is String's inverse, used to read a persisted kind back.
//
// An unrecognized name — an accounts.toml written before the field existed, a
// typo, or a kind a future release adds — reads as a subscription for the same
// reason Classify's default does: credit is the side that spends money.
func ParseKind(name string) Kind {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "credit":
		return KindCredit
	case "api-key":
		return KindAPIKey
	default:
		return KindSubscription
	}
}

// UsageShape is the part of a usage response that matters for classification.
type UsageShape struct {
	// HasSubscriptionWindows is true when the usage response carried a
	// five_hour or seven_day window.
	HasSubscriptionWindows bool
	// ExtraUsageEnabled is extra_usage.is_enabled.
	ExtraUsageEnabled bool
}

// isMeteredBilling reports whether a billing_type means "pays per unit". An
// unrecognized value is not treated as evidence, so a new value Anthropic adds
// falls through to the side that does not spend.
func isMeteredBilling(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "usage_based", "credit", "prepaid", "pay_as_you_go":
		return true
	}
	return false
}

// Classify decides how an account is metered.
//
// Subscription windows win outright: an account with an active five-hour or
// seven-day window is ranked on those windows, even when overage credits are
// also enabled, because credits are not spent while a window still has room
// (spec §5).
//
// The default when there is no evidence is KindSubscription, deliberately.
// Guessing KindCredit for an account we cannot read would put it on the far
// side of the credit gate, which is the side that spends money.
func Classify(p *Profile, u UsageShape, isAPIKey bool) Kind {
	if isAPIKey {
		return KindAPIKey
	}
	if u.HasSubscriptionWindows {
		return KindSubscription
	}
	// Only the usage endpoint's extra_usage.is_enabled is credit evidence. The
	// profile's has_extra_usage_enabled is the organization's overage switch,
	// which subscription orgs turn on too, so it says nothing about how the
	// account is metered — and the callers that never fetch usage would
	// otherwise file every Max account with overage on the credit side of the
	// gate.
	if u.ExtraUsageEnabled {
		return KindCredit
	}
	if p != nil && isMeteredBilling(p.BillingType) {
		return KindCredit
	}
	return KindSubscription
}
