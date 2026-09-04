package config

import "strings"

// Effective is the configuration the engine actually runs on.
//
// It is the parsed file everywhere except under hover, where one key changes:
// probe_unknown is forced on. Hover derives every threshold from how far
// through its own window each account is, and a window that has never been
// spent against reports no reset -- so it has no share elapsed, falls back to a
// fixed 80, and stays there. Only a turn spent against that window puts a reset
// on it, so probe_unknown off under hover is the one state hover cannot climb
// out of by itself.
//
// It is deliberately NOT applied by Parse or by Document.Config. `ccdad config
// list` reads those, and a listing that printed `probe_unknown true` beside the
// word `file` for a file that says `false` would be a falsehood in the column
// whose whole job is provenance. The listing marks the key overridden instead,
// and this is where the override actually happens.
func (c Config) Effective() Config {
	if !c.Hover {
		return c
	}
	c.ProbeUnknown = true
	return c
}

// KeyHover is the compatibility key `ccdad strategy` writes for the hover
// policy. It is exported so the command does not duplicate its on-disk name.
const KeyHover = keyHover

// KeyManual is the compatibility key `ccdad strategy` writes for the manual
// policy, exported for the same reason as KeyHover.
const KeyManual = keyManual

// KeyStrategy is the automatic-policy key written by `ccdad strategy`.
const KeyStrategy = keyStrategy

// hoverOverrides is every key hover derives for itself, so a value in the file
// stops being read the moment hover is on.
//
// It is a table rather than a condition because the CLI has to be able to MARK
// each of them. A listing that quietly stopped applying a number the user tuned
// is the same failure as an accepted typo -- a setting that does nothing --
// reached from the other side.
var hoverOverrides = map[string]bool{
	keyThreshold:          true,
	keyHysteresisPct:      true,
	keyHeadroomRatio:      true,
	keyCooldown:           true,
	keyRecoveryHysteresis: true,
	keyStrategy:           true,
	keyPreemptLead:        true,
	keyProbeUnknown:       true,
	keyCreditThreshold:    true,
}

// hoverHonours is the other half, and the reason this is two tables rather than
// one negation: TestEveryKeyIsClassifiedAgainstHover fails for a key in neither,
// so a key added later gets a decision instead of the permissive default.
//
// credit.max_auto_spend is here because unattended overage needs two independent
// opt-ins and this is one of them. An opt-in a mode supplies on the user's
// behalf is not an opt-in, so "fully automatic" stops short of "fully automatic
// spending". hover is here because it is the switch itself.
//
// mcp_switch_without_elicitation is here because hover is a policy for the
// SWITCHING ENGINE and this key is a permission for a different surface
// entirely. A mode that derived it would be deciding, on the user's behalf,
// that an unattended switch may also be an unconfirmed one.
//
// update_check is here for the same reason one step further out: hover is a
// policy for the switching engine, and this key is whether the daemon may make
// a request at all. A mode that derived it would be deciding, on the user's
// behalf, that fully automatic also means fully connected.
//
// The two display keys are here because hover is a policy for WHICH ACCOUNT is
// live, and nothing about how a screen is painted follows from that. A mode
// that derived them would be answering a question about the user's terminal out
// of a fact about their quota, and `ccdad config list` would then mark a theme
// the user chose as one hover had taken over -- which would be a lie in the
// column whose whole job is to say what is being read.
//
// The four Codex keys are here for the same reason, one lane over: hover is a
// policy for the CLAUDE switching engine, and it derives nothing about a Codex
// account -- there is no window, no reset and no ranking on that lane for it to
// compute a number from. A mode that derived them would be answering a
// question about a different provider out of a fact about Claude's quota.
//
// manual is here because the two modes answer different questions and compose
// rather than conflict. Hover decides WHAT THE NUMBERS ARE; manual decides
// WHETHER THE ENGINE ACTS ON THEM. A hover that derived manual would be
// deciding, out of a fact about quota, that the user did not mean the mode they
// switched on -- and the derived thresholds are the very thing a manual-mode
// user is watching, so the two are useful together rather than exclusive.
var hoverHonours = map[string]bool{
	keyMaxAutoSpend:                true,
	keyHover:                       true,
	keyManual:                      true,
	keyMCPSwitchWithoutElicitation: true,
	keyUpdateCheck:                 true,
	keyTUITheme:                    true,
	keyTUIGlyphs:                   true,
	keyCodexThreshold:              true,
	keyCodexBinary:                 true,
	keyCodexProxyPort:              true,
	keyCodexCrossAccountReplay:     true,
}

// HoverOverrides reports whether hover derives this key's value for itself,
// which is to say whether the value in the file has stopped being read.
//
// Every key under the window-threshold section answers true, listed or not: a
// well-formed window name this build does not know still round-trips through the
// file, and the annotation has to be right for it whenever a caller asks.
func HoverOverrides(key string) bool {
	if strings.HasPrefix(key, windowThresholdPrefix) {
		return true
	}
	return hoverOverrides[key]
}
