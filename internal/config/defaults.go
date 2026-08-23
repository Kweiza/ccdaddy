package config

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// defaultPreemptLead is how far ahead of a projected exhaustion the engine
// switches, on top of the blind interval between two polls.
//
// Two minutes is two urgent poll intervals — pollpolicy.UrgentInterval is 60 s,
// the fastest cadence the endpoint's roughly 28-30 requests per identity per
// rolling hour will bear. The horizon already counts the time until the next
// reading; this is what is left over for the switch to be decided, written, and
// picked up by the next session to start. A lead shorter than one poll interval
// would routinely be overtaken between two readings, which is the case the
// whole mechanism exists for.
//
// It lives HERE and not in the engine. strategy reads a zero PreemptLead as the
// pre-emptive switch off, so a default constant on that side would turn the
// mechanism back on for a caller that meant to leave it off.
const defaultPreemptLead = 2 * time.Minute

// The defaults are the ENGINE's own defaults, never a second copy of them.
//
// strategy.Config's zero value is already the full anti-flap default set and
// strategy.Options defaults its threshold the same way, so a number written
// again here could drift from the one the engine falls back to — and then
// omitting a key from config.toml would change behaviour by omitting it, which
// is the one thing a defaults table must never do.
//
// MaxAutoSpend is the exception in the other direction: 0 is an ANSWER rather
// than an omission. The credit gate requires two independent opt-ins for
// unattended spending and this is one of them, so the default refuses.
//
// PreemptLead is an answer in the same shape and the reason its number is
// config's own: 0 there means the pre-emptive switch is off, which is a choice
// a user may make — it is an opt-out, not an anti-flap mechanism that must
// never be switched off — so the engine must not re-default it, and something
// has to say 2m. This is that something.
//
// ProbeUnknown and Hover are answers too. ProbeUnknown is ON because a window
// that has never been used reports a null reset, which leaves the account
// invisible to the ranking, and the only way to obtain a reading is to spend
// against the window — one turn, which is why it is a key at all rather than
// unconditional. Hover is OFF because it overrides every number the user
// tuned, and a mode that ignores the config file has to be asked for.
//
// WindowThreshold is deliberately absent. Nil IS the default — every window
// using Threshold — and an empty map here would allocate one per call to say
// the same thing.
func Defaults() Config {
	return Config{
		Threshold:          strategy.DefaultThreshold,
		HysteresisPct:      strategy.DefaultHysteresisPct,
		HeadroomRatio:      strategy.DefaultHeadroomRatio,
		Cooldown:           strategy.DefaultCooldown,
		RecoveryHysteresis: strategy.DefaultRecoveryHysteresis,
		Strategy:           strategy.StrategyHeadroom,
		MaxAutoSpend:       0,
		CreditThreshold:    strategy.DefaultCreditThreshold,
		PreemptLead:        defaultPreemptLead,
		ProbeUnknown:       true,
		Hover:              false,
	}
}
