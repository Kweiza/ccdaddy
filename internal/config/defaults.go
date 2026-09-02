package config

import (
	"time"

	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// defaultPreemptLead is how far ahead of a projected exhaustion the engine
// switches, on top of the blind interval between two polls.
//
// Six minutes is two of the cadence an at-risk account actually polls at.
//
// The number is derived, and the derivation is the point. It used to be two
// minutes, on the reading that the fastest cadence an at-risk account sees is
// pollpolicy.UrgentInterval at 60 s. That stopped being true when the danger
// band — the cadence a live account close to its window's ceiling is actually
// on — moved from 60 s to pollpolicy.DangerInterval at 180 s. Left at two
// minutes the lead would be SHORTER than one poll interval, which this comment
// has always said is the case the whole mechanism exists to prevent: the
// projection is then routinely overtaken between two readings and the switch
// lands after the session has already been cut off.
//
// The horizon already counts the time until the next reading; this is what is
// left over for the switch to be decided, written, and picked up by the next
// session to start.
//
// It lives HERE and not in the engine. strategy reads a zero PreemptLead as the
// pre-emptive switch off, so a default constant on that side would turn the
// mechanism back on for a caller that meant to leave it off.
const defaultPreemptLead = 6 * time.Minute

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
// UpdateCheck is an answer too, and the only one behind which there is a host
// other than api.anthropic.com. It is ON because a user who never edits this
// file is exactly the user who will otherwise never hear that a release
// shipped.
//
// TUITheme and TUIGlyphs are answers in a third shape. Their real default is a
// MEASUREMENT of the terminal that will draw the screen -- how dark its
// background is, what its console code page says it can carry -- and this
// function has no terminal. `auto` is the word that says "measure it", so it is
// the honest default rather than a placeholder standing in for one. Naming a
// palette here would be one machine's answer written down for every machine.
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
		// False refuses. The one thing this key must never do is arrive
		// switched on for someone who has not read what it is: it is the
		// permission that lets an MCP client rewrite the live login without
		// asking the person at the keyboard.
		MCPSwitchWithoutElicitation: false,
		// True, and this is the one default in this table that makes a network
		// request nobody asked for. It is deliberate: the request is one per
		// day per store to the host the machine already used to install ccdad,
		// it carries the version and nothing else, and the alternative is a
		// user running a build with a known bug in it because nothing ever told
		// them otherwise. `update_check = false` is how a machine that may not
		// call out says so.
		UpdateCheck: true,
		TUITheme:    defaultTUITheme,
		TUIGlyphs:   defaultTUIGlyphs,
		Codex: CodexConfig{
			// The same number as the engine's own default threshold, and
			// written here rather than taken from strategy for the reason
			// MaxAutoSpend is written here: strategy has no Codex axis to
			// default, so there is no engine constant to defer to.
			Threshold: 80,
			// Empty means the first codex on PATH that is not ccdad's own
			// shim. Naming a binary here would be one machine's answer written
			// down for every machine.
			Binary: "",
			// 0 means resolve one. A number here is a machine saying it needs
			// the port to be stable, which is a choice rather than a default.
			ProxyPort: 0,
			// False, and this is an answer rather than caution: every codex
			// request carries the whole thread including the reasoning content
			// the previous account produced, so a mid-thread replay bills a
			// second account for a thread the first started.
			CrossAccountReplay: false,
		},
	}
}
