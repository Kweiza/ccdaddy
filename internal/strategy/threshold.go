package strategy

import "github.com/Kweiza/ccdaddy/internal/usage"

// Thresholds is every threshold one ranking pass may consult, in one value.
//
// It is a value type with a nil-safe map, so a caller never branches on
// presence: the zero Thresholds is the built-in default set, and a nil PerWindow
// is the ordinary state rather than a missing one.
type Thresholds struct {
	// Default is the threshold for a window with no entry of its own, which is
	// every window until a user writes a table.
	Default float64
	// PerWindow is the per-window table, keyed by the window name the wire
	// uses — scoped names included — so the map a user's configuration builds
	// needs no translation between what was typed and what a snapshot reports.
	PerWindow map[usage.WindowName]float64
	// Credit is the utilization percent above which a credit-metered account
	// counts as spent. It is a separate number from Default because the two
	// meter different things: Default is a share of a plan window, this is a
	// share of a credit balance.
	Credit float64
}

// For is the threshold for one window: its own entry when it has one, Default
// when it does not.
//
// Both fallbacks default rather than being read literally, and it matters. A
// zero threshold read literally means "over threshold if utilization > 0", so an
// account with a single percent used counts as spent — which flows straight into
// SubscriptionExhausted, the input that opens the credit gate. A zero-valued
// Thresholds would therefore fail OPEN on money, against everything the credit
// gate stands for. Defaulting makes the omission harmless instead, and a
// non-positive entry in the table is treated as the same omission for the same
// reason.
func (t Thresholds) For(n usage.WindowName) float64 {
	if v, ok := t.PerWindow[n]; ok && v > 0 {
		return v
	}
	if t.Default <= 0 {
		return DefaultThreshold
	}
	return t.Default
}

// CreditThreshold is the credit meter's threshold, defaulted the same way and
// for the same fail-open-on-money reason.
func (t Thresholds) CreditThreshold() float64 {
	if t.Credit <= 0 {
		return DefaultCreditThreshold
	}
	return t.Credit
}
