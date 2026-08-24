package config

import (
	"strings"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The key namespace, in one table.
//
// It is closed. `ccdad config set` refuses a name that is not here (exit 2,
// because an accepted typo is a config that silently does nothing), and the
// loader ignores one so a newer ccdad's file cannot stop an older one. Both
// answers come from this list, so a key cannot exist for one and not the other.
//
// NO SECRET IS SETTABLE. Nothing token-shaped may be added: this file is
// 0600-but-plaintext, it is the file people paste into an issue, and
// credentials belong in the store's credentials directory. TestTheKeySetIsClosed
// is the gate — it names every key, so adding one is a deliberate edit in two
// places rather than a line in a table.
const (
	keyThreshold          = "threshold"
	keyHysteresisPct      = "hysteresis_pct"
	keyHeadroomRatio      = "headroom_ratio"
	keyCooldown           = "cooldown"
	keyRecoveryHysteresis = "recovery_hysteresis"
	keyPreemptLead        = "preempt_lead"
	keyStrategy           = "strategy"
	keyProbeUnknown       = "probe_unknown"
	keyHover              = "hover"

	// keyMCPSwitchWithoutElicitation is the only key in this file that governs
	// no part of the switching engine. It is here, in the file a person edits
	// by hand, precisely BECAUSE of what it governs: it is one of the two ways
	// the person at the keyboard allows ccdad's MCP server to move the live
	// login on a client that cannot ask them first, and the whole value of it
	// is that a model talking to that server has no way to write it.
	keyMCPSwitchWithoutElicitation = "mcp_switch_without_elicitation"

	keyCreditThreshold = "credit.threshold"
	keyMaxAutoSpend    = "credit.max_auto_spend"

	// creditSection and windowThresholdSection are the two tables keys nest
	// under; every other key is top-level.
	creditSection = "credit"

	// windowThresholdSection carries one threshold per rate-limit window. It is
	// a table of its own rather than `threshold.<window>` because `threshold`
	// is already a scalar: go-toml refuses to reopen a scalar's name as a table
	// and rejects the whole document with "key threshold already exists as a
	// value". Dropping the scalar instead is not available either — the keys in
	// this file are a compatibility commitment, and `threshold` is what every
	// window with no key of its own falls back to.
	windowThresholdSection = "window_threshold"

	// windowThresholdPrefix is what a key in that table begins with. Both the
	// predicate and the refusal cut on it, so the dot cannot be attached in one
	// place and forgotten in the other.
	windowThresholdPrefix = windowThresholdSection + "."
)

// KeyMCPSwitchWithoutElicitation is the second key a package outside this one
// names directly. internal/mcpsrv prints it in the refusal a model reads when
// nobody can be asked to confirm a switch, and a name spelled twice is exactly
// the drift keys.go exists to prevent -- here it would print a key that
// `ccdad config set` then refuses.
const KeyMCPSwitchWithoutElicitation = keyMCPSwitchWithoutElicitation

// Keys lists every settable key this release knows by name, in file order,
// which is also the order `ccdad config list` prints them in. A CLI builds its
// help and its error messages from this, so a key added here cannot be
// forgotten in either. The top-level keys come first and the credit table last,
// so the listing groups the sub-table the way a hand-written file does.
//
// It is not the whole settable surface. window_threshold takes one key per
// rate-limit window and a scoped window is named after a model or a surface the
// server invented, so that half cannot be listed in advance. Document.Keys is
// the listing that adds the ones a file actually names.
func Keys() []string {
	return []string{
		keyThreshold,
		keyHysteresisPct,
		keyHeadroomRatio,
		keyCooldown,
		keyRecoveryHysteresis,
		keyPreemptLead,
		keyStrategy,
		keyProbeUnknown,
		keyHover,
		keyMCPSwitchWithoutElicitation,
		keyCreditThreshold,
		keyMaxAutoSpend,
	}
}

// isKnownKey answers for both halves of the namespace, which are closed in two
// different ways: the list above is closed by name, and window_threshold is
// closed one level down, by what a window may be called.
func isKnownKey(name string) bool {
	if strings.HasPrefix(name, windowThresholdPrefix) {
		_, ok := windowOf(name)
		return ok
	}
	for _, k := range Keys() {
		if k == name {
			return true
		}
	}
	return false
}

// windowOf is the window a window_threshold key names, and whether this release
// accepts it. A key outside the section answers false, which is what keeps the
// closed half closed.
//
// cinder_cove is a window the endpoint really reports and is refused all the
// same: its resets_at is an expiry rather than a rollover, so RateLimitWindows
// leaves it out of the ranking and a threshold on it would govern nothing.
// usage.ValidWindowName is the one place that rule is spelled, so config
// validation and the snapshot parser cannot drift apart.
func windowOf(key string) (usage.WindowName, bool) {
	name, inSection := strings.CutPrefix(key, windowThresholdPrefix)
	if !inSection {
		return "", false
	}
	w := usage.WindowName(name)
	return w, usage.ValidWindowName(w) == nil
}

// isKnownSection reports whether a table is one this release nests keys under.
// A table it does not know is reported whole rather than key by key, because
// naming `[future].a` and `[future].b` separately says nothing more than
// naming `future` once.
func isKnownSection(name string) bool {
	return name == creditSection || name == windowThresholdSection
}
