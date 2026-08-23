package config

import "github.com/Kweiza/ccdaddy/internal/strategy"

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
func Defaults() Config {
	return Config{
		Threshold:          strategy.DefaultThreshold,
		HysteresisPct:      strategy.DefaultHysteresisPct,
		HeadroomRatio:      strategy.DefaultHeadroomRatio,
		Cooldown:           strategy.DefaultCooldown,
		RecoveryHysteresis: strategy.DefaultRecoveryHysteresis,
		Strategy:           strategy.StrategyHeadroom,
		MaxAutoSpend:       0,
	}
}
