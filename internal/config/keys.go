package config

// The key namespace, in one table.
//
// It is closed. `ccdad config set` refuses a name that is not here (§9.3's
// exit 2, because an accepted typo is a config that silently does nothing), and
// the loader ignores one so a newer ccdad's file cannot stop an older one. Both
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
	keyStrategy           = "strategy"
	keyMaxAutoSpend       = "credit.max_auto_spend"

	// creditSection is the one table §7 groups keys under.
	creditSection = "credit"
)

// Keys lists every settable key in file order, which is also the order `ccdad
// config list` prints them in. A CLI builds its help and its error messages
// from this, so a key added here cannot be forgotten in either.
func Keys() []string {
	return []string{
		keyThreshold,
		keyHysteresisPct,
		keyHeadroomRatio,
		keyCooldown,
		keyRecoveryHysteresis,
		keyStrategy,
		keyMaxAutoSpend,
	}
}

func isKnownKey(name string) bool {
	for _, k := range Keys() {
		if k == name {
			return true
		}
	}
	return false
}

// isKnownSection reports whether a table is one this release nests keys under.
// A table it does not know is reported whole rather than key by key, because
// naming `[future].a` and `[future].b` separately says nothing more than
// naming `future` once.
func isKnownSection(name string) bool { return name == creditSection }
