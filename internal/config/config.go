// Package config is ~/.ccdad/config.toml: the auto-switch engine's knobs, their
// defaults, and the re-read the daemon's tick loop performs every second.
//
// KEY STYLE, and this is a compatibility commitment rather than a preference.
// Every key here is SNAKE_CASE, which is the style accounts.toml already uses
// (`active_uuid`): `hysteresis_pct` and `max_auto_spend`, never a camelCase
// spelling of either. `credit` is the one key group that keeps its own table,
// so the ceiling is `credit.max_auto_spend`.
//
// `ccdad config get|set|list` names these keys and prints these spellings, so
// changing one later breaks scripts. Add keys; do not rename them.
//
// There is deliberately no `version` key. accounts.toml carries one because
// ccdad writes it and would have to migrate it; this file is written by hand as
// often as by `ccdad config set`, and a version a reader never checks is a
// promise nothing keeps. Unknown keys round-trip and are ignored, which is what
// makes adding one later — `version` included — a widening rather than a break.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// FileName is the config file's basename inside the ccdad store.
const FileName = "config.toml"

// CodexConfig is the [codex] table: the four knobs the Codex lane, the
// launcher and the proxy read.
//
// It is a nested struct rather than four fields on Config with codex- prefixes,
// because the file has a table and the two shapes must agree -- a reader
// looking for what governs Codex should find one place in the struct and one
// heading in the file.
type CodexConfig struct {
	// Threshold is the utilization percent above which a Codex account counts
	// as spent. A second number rather than a use of Config.Threshold, for the
	// reason CreditThreshold is one: the two providers meter different
	// quantities over different windows.
	Threshold float64
	// Binary names the real codex, for a machine where the PATH walk should
	// not decide. Empty is the ordinary state: the first codex on PATH that is
	// not ccdad's own shim.
	Binary string
	// ProxyPort pins the loopback port. 0 means resolve one -- the recorded
	// port, or a default derived from the store's own path -- which is the
	// ordinary state. A configured port that cannot be bound fails daemon
	// start loudly; a resolved one falls back.
	ProxyPort int
	// CrossAccountReplay allows a 429 in the MIDDLE of a thread to be replayed
	// on another account.
	//
	// It defaults to false, and that is a fact about how codex sends a turn
	// rather than caution: every request carries the whole history including
	// the reasoning content the previous account produced, so a replay bills a
	// second account for a thread the first started and hands it material it
	// did not generate. The default answer to a mid-thread 429 is to return it
	// and let the user start a new thread, which lands on the new account
	// cleanly.
	//
	// The default is provisional rather than settled: the paragraph above is
	// reasoned from the shape of a codex request and is unmeasured, so a
	// measurement made before release may turn it on.
	CrossAccountReplay bool
}

// Config is the effective configuration: what the file says, over the defaults.
//
// No field is a pointer, and that is the property being kept: absence is
// resolved during parsing and never leaves this package, so no consumer has to
// ask whether a field was set. WindowThreshold is a map and keeps the same
// promise — a nil map is not "unset", it is every window using Threshold, which
// is the answer a missing key gives too, so nothing downstream needs a nil
// check.
//
// Being comparable with == was a CONSEQUENCE of having no pointers rather than
// the goal, and the map ends it. Compare two configs with Equal.
type Config struct {
	// Threshold is the utilization percent above which an account counts as
	// spent, for any window with no threshold of its own.
	Threshold float64
	// HysteresisPct is the additive anti-flap margin, in points of headroom.
	HysteresisPct float64
	// HeadroomRatio is the multiplicative anti-flap margin on the same axis.
	HeadroomRatio float64
	// Cooldown is the anti-flap minimum gap between two switches.
	Cooldown time.Duration
	// RecoveryHysteresis is the anti-flap margin on the recovery axis.
	RecoveryHysteresis time.Duration
	// Strategy is which question the ranking asks.
	Strategy strategy.Strategy
	// MaxAutoSpend is the credit gate's ceiling, in the currency's major unit.
	// 0 refuses, and is the default.
	MaxAutoSpend float64
	// WindowThreshold is a threshold per window, overriding Threshold for the
	// windows named in it. Nil is every window using Threshold.
	//
	// Nothing mutates it after Parse builds it, which is what makes handing the
	// same map to the ranking safe where a Config crosses goroutines — the
	// daemon copies the struct under a mutex, and copying a struct copies the
	// map header, not the map.
	WindowThreshold map[usage.WindowName]float64
	// CreditThreshold is the utilization percent above which a credit-metered
	// account counts as spent. It is a second number rather than Threshold
	// because the two are different axes: credits are metered spending and a
	// subscription window is metered quota, so a user who tightens one has said
	// nothing about the other.
	CreditThreshold float64
	// PreemptLead is how far ahead of a projected exhaustion the engine
	// switches, on top of the blind interval between two polls. 0 is the
	// pre-emptive switch off, which is a value a user may choose.
	PreemptLead time.Duration
	// ProbeUnknown is whether the daemon may spend one turn against a window
	// that has never been used, to get the reset time such a window reports as
	// null.
	ProbeUnknown bool
	// Hover is the fully automatic mode: thresholds derived from pace, and
	// every tuning key ignored.
	Hover bool
	// Manual is hover's opposite number and the mode nothing else in this file
	// can express: the engine keeps polling, ranking and recording, and never
	// moves the login. `ccdad switch` is untouched — it is a policy for the auto
	// engine, not a lock.
	//
	// It is a MODE rather than a knob, so it is a plain bool with no default to
	// fall back to, exactly as Hover is. Disabling every account reaches the
	// same silence by a different door and costs the probes, the forecast and
	// the listing to get there; this key costs none of them.
	Manual bool
	// MCPSwitchWithoutElicitation allows ccdad's MCP server to rewrite the live
	// login on a client that cannot ask the person at the keyboard first.
	//
	// It is the only field here the engine never reads, and the reason it lives
	// in this file rather than in an option the caller passes is the threat it
	// answers: a model reaches that server through tool arguments and through
	// nothing else, so a permission that can only be granted by editing this
	// file -- or by an environment variable on the server's own process -- is
	// one the model cannot grant itself in the call it is making.
	//
	// It defaults to false, which refuses.
	MCPSwitchWithoutElicitation bool
	// UpdateCheck is whether the daemon may ask, once a day, what the newest
	// released ccdad is.
	//
	// It is the only field here behind which there is a request to a host other
	// than api.anthropic.com, and it defaults to TRUE: a user who never edits
	// this file would otherwise never hear that a fix shipped. The daemon
	// publishes what it saw and never a verdict, so this key switches off a
	// request rather than a recommendation.
	UpdateCheck bool
	// TUITheme is which palette ccdad's own screens paint with, held as a
	// STRING rather than as the palette package's own type.
	//
	// The string is the deliberate half. A typed field would make this package
	// the one that decides what a theme IS, and it is not: resolving `auto`
	// needs the terminal's background, which the daemon that loads this file has
	// never looked at and must not look at -- it has no terminal, and a daemon
	// that queried one would be answering a question about a screen nobody is
	// watching. So the file's answer travels as the word the user wrote, and the
	// command that owns a terminal turns it into a palette. What this package
	// owes is that the word is one the palette layer will accept, which is what
	// validTheme checks at the moment the word arrives.
	TUITheme string
	// TUIGlyphs is which glyph set those screens draw with: auto, unicode or
	// ascii. It is a string for the reason above and for one more: `auto`
	// resolves against the console's code page and against an environment
	// variable read once at process start, and the daemon has neither.
	TUIGlyphs string
	// Codex is the [codex] table. See CodexConfig.
	Codex CodexConfig
}

// Equal is == for a Config, which the map field ended.
//
// The map compares by CONTENT, and a nil map equals an empty one: both say
// "every window uses Threshold", so reporting them as different configurations
// would have the daemon announce a config change nobody made.
//
// Every field is listed. TestEqualComparesEveryFieldOfConfig walks the struct
// by reflection and fails on a field this method forgot, because the symptom of
// a forgotten one is silent: a config change the reload path decides is not a
// change.
func (c Config) Equal(o Config) bool {
	return c.Threshold == o.Threshold &&
		c.HysteresisPct == o.HysteresisPct &&
		c.HeadroomRatio == o.HeadroomRatio &&
		c.Cooldown == o.Cooldown &&
		c.RecoveryHysteresis == o.RecoveryHysteresis &&
		c.Strategy == o.Strategy &&
		c.MaxAutoSpend == o.MaxAutoSpend &&
		c.CreditThreshold == o.CreditThreshold &&
		c.PreemptLead == o.PreemptLead &&
		c.ProbeUnknown == o.ProbeUnknown &&
		c.Hover == o.Hover &&
		c.Manual == o.Manual &&
		c.MCPSwitchWithoutElicitation == o.MCPSwitchWithoutElicitation &&
		c.UpdateCheck == o.UpdateCheck &&
		c.TUITheme == o.TUITheme &&
		c.TUIGlyphs == o.TUIGlyphs &&
		c.Codex == o.Codex &&
		maps.Equal(c.WindowThreshold, o.WindowThreshold)
}

// Thresholds is every threshold one ranking pass may consult, in one value.
//
// It is the accessor every consumer outside this package uses, and the three
// fields are not read individually: a caller that took Threshold on its own
// would be asking the old single-threshold question and would quietly disagree
// with the engine the moment a user wrote one per-window key.
//
// The map is handed over rather than copied. The engine only reads it, nothing
// mutates it after Parse built it, and a copy would allocate on every cadence
// for a table that changes only when the file does.
func (c Config) Thresholds() strategy.Thresholds {
	return strategy.Thresholds{
		Default:   c.Threshold,
		PerWindow: c.WindowThreshold,
		Credit:    c.CreditThreshold,
	}
}

// StrategyConfig is the anti-flap half, in the shape strategy.Decide takes.
func (c Config) StrategyConfig() strategy.Config {
	return strategy.Config{
		HysteresisPct:      c.HysteresisPct,
		HeadroomRatio:      c.HeadroomRatio,
		Cooldown:           c.Cooldown,
		RecoveryHysteresis: c.RecoveryHysteresis,
		MaxAutoSpend:       c.MaxAutoSpend,
		Manual:             c.Manual,
	}
}

// RankOptions is the ranking half, for a pass evaluated at now.
//
// Horizon is deliberately left at its zero value: no configuration key names
// the horizon, so strategy.DefaultRecoveryHorizon stays the single place it is
// decided. PreemptLead is the opposite case and is always carried: the engine
// reads a zero as the pre-emptive switch OFF, so a lead left behind here would
// silently disable a mechanism the user configured.
//
// WindowThreshold is handed over rather than copied, for the reason Thresholds
// gives: the ranking only reads it and nothing mutates it after Parse.
//
// Hover is carried through rather than resolved here. The mode's effect depends
// on the POOL -- how many accounts have a reading, and how far through its own
// window each one is -- and this package has neither. All that travels is the
// bit; the engine derives everything from it.
func (c Config) RankOptions(now time.Time) strategy.Options {
	return strategy.Options{
		Now:             now,
		Threshold:       c.Threshold,
		WindowThreshold: c.WindowThreshold,
		CreditThreshold: c.CreditThreshold,
		PreemptLead:     c.PreemptLead,
		Strategy:        c.Strategy,
		Hover:           c.Hover,
	}
}

// Path is where the config lives.
func Path() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, FileName), nil
}

// storeRoot refuses a relative store root, exactly as store.Open and
// strategy.LoadState do. ccpath.StoreHome now reports an unresolvable home
// rather than degrading to a relative path, so what is left for this guard is
// the case it cannot report: a CCDAD_HOME that is itself relative. A relative
// root means the config comes from whatever directory ccdad happened to be run
// in — a different answer per invocation, which for a threshold is worse than
// no answer.
func storeRoot() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("the ccdad store resolved to the relative path %q; set CCDAD_HOME to an absolute path", root)
	}
	return root, nil
}

// fileShape is the on-disk document, decoded with a POINTER per key.
//
// go-toml decodes an absent key as the zero value, so a plain struct cannot
// tell "unset" from "explicitly 0" — and the two differ in opposite directions
// here. For max_auto_spend a 0 is the refusing default and is safe either way;
// for hysteresis_pct and the cooldowns a 0 silently switches an anti-flap
// mechanism off, and none of them has a disabled setting. Pointers make both
// cases decidable, so absence takes the default and an explicit 0 is validated
// like any other value.
type fileShape struct {
	Threshold          *float64 `toml:"threshold"`
	HysteresisPct      *float64 `toml:"hysteresis_pct"`
	HeadroomRatio      *float64 `toml:"headroom_ratio"`
	Cooldown           *string  `toml:"cooldown"`
	RecoveryHysteresis *string  `toml:"recovery_hysteresis"`
	PreemptLead        *string  `toml:"preempt_lead"`
	Strategy           *string  `toml:"strategy"`
	ProbeUnknown       *bool    `toml:"probe_unknown"`
	Hover              *bool    `toml:"hover"`
	Manual             *bool    `toml:"manual"`

	// Apart from the rest because it is the one key here that is not an engine
	// knob, and a pointer for the same reason as the others: absence has to be
	// distinguishable from an explicit false, which is a person taking the
	// permission back rather than never having granted it.
	MCPSwitchWithoutElicitation *bool `toml:"mcp_switch_without_elicitation"`

	// A pointer for the reason every key here is one: absence has to be
	// distinguishable from an explicit false, which is a person switching the
	// daily release check off rather than never having had it on.
	UpdateCheck *bool `toml:"update_check"`

	WindowThreshold map[string]float64 `toml:"window_threshold"`
	Credit          *creditFile        `toml:"credit"`

	// TUI is a pointer to a table rather than two pointers at top level, and
	// absence has to be decidable at both levels: an absent [tui] and a [tui]
	// that names only one of its two keys are different documents, and the key
	// that was not written keeps its own default in each of them. Without the
	// table here the loader would report `auto` for a file that says `ansi`,
	// and `ccdad config list` would print the default beside the word `file`.
	TUI *tuiFile `toml:"tui"`

	// A pointer for the same reason TUI is one: an absent [codex] and a
	// [codex] that names one key are different documents.
	Codex *codexFile `toml:"codex"`
}

type creditFile struct {
	MaxAutoSpend *float64 `toml:"max_auto_spend"`
	Threshold    *float64 `toml:"threshold"`
}

// Every field is a pointer for the reason every key in this file is one:
// absence has to be distinguishable from an explicit zero or false, or a
// [codex] table naming one key would reset the other three.
type codexFile struct {
	Threshold          *float64 `toml:"threshold"`
	Binary             *string  `toml:"binary"`
	ProxyPort          *int     `toml:"proxy_port"`
	CrossAccountReplay *bool    `toml:"cross_account_replay"`
}

type tuiFile struct {
	Theme  *string `toml:"theme"`
	Glyphs *string `toml:"glyphs"`
}

// Parse turns a config document into the effective configuration.
//
// It is the whole loader minus the filesystem, which is what lets every trap
// the credit gate sets be pinned by a test that writes no file. A document that
// fails validation yields the ZERO Config alongside its error and never a
// partly applied one: half a config file is not a configuration, and the caller
// that falls back to the defaults has to be sure it is falling back to all of
// them.
func Parse(raw []byte) (Config, error) {
	var f fileShape
	if err := toml.Unmarshal(raw, &f); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", FileName, err)
	}

	cfg := Defaults()
	if err := applyFloat(&cfg.Threshold, f.Threshold, keyThreshold, validThreshold); err != nil {
		return Config{}, err
	}
	if err := applyFloat(&cfg.HysteresisPct, f.HysteresisPct, keyHysteresisPct, validHysteresisPct); err != nil {
		return Config{}, err
	}
	if err := applyFloat(&cfg.HeadroomRatio, f.HeadroomRatio, keyHeadroomRatio, validHeadroomRatio); err != nil {
		return Config{}, err
	}
	if err := applyDuration(&cfg.Cooldown, f.Cooldown, keyCooldown); err != nil {
		return Config{}, err
	}
	if err := applyDuration(&cfg.RecoveryHysteresis, f.RecoveryHysteresis, keyRecoveryHysteresis); err != nil {
		return Config{}, err
	}
	if err := applyPreemptLead(&cfg.PreemptLead, f.PreemptLead); err != nil {
		return Config{}, err
	}
	if f.Strategy != nil {
		s, err := parseStrategy(*f.Strategy)
		if err != nil {
			return Config{}, err
		}
		cfg.Strategy = s
	}
	applyBool(&cfg.ProbeUnknown, f.ProbeUnknown)
	applyBool(&cfg.Hover, f.Hover)
	applyBool(&cfg.Manual, f.Manual)
	applyBool(&cfg.MCPSwitchWithoutElicitation, f.MCPSwitchWithoutElicitation)
	applyBool(&cfg.UpdateCheck, f.UpdateCheck)
	if err := applyWindowThresholds(&cfg, f.WindowThreshold); err != nil {
		return Config{}, err
	}
	if f.Credit != nil {
		if err := applyFloat(&cfg.MaxAutoSpend, f.Credit.MaxAutoSpend, keyMaxAutoSpend, validMaxAutoSpend); err != nil {
			return Config{}, err
		}
		if err := applyFloat(&cfg.CreditThreshold, f.Credit.Threshold, keyCreditThreshold, validThreshold); err != nil {
			return Config{}, err
		}
	}
	if f.TUI != nil {
		if err := applyString(&cfg.TUITheme, f.TUI.Theme, keyTUITheme, validTheme); err != nil {
			return Config{}, err
		}
		if err := applyString(&cfg.TUIGlyphs, f.TUI.Glyphs, keyTUIGlyphs, validGlyphs); err != nil {
			return Config{}, err
		}
	}
	if f.Codex != nil {
		if err := applyFloat(&cfg.Codex.Threshold, f.Codex.Threshold, keyCodexThreshold, validThreshold); err != nil {
			return Config{}, err
		}
		if f.Codex.Binary != nil {
			cfg.Codex.Binary = *f.Codex.Binary
		}
		if f.Codex.ProxyPort != nil {
			if err := validProxyPort(*f.Codex.ProxyPort); err != nil {
				return Config{}, fmt.Errorf("%s in %s: %w", keyCodexProxyPort, FileName, err)
			}
			cfg.Codex.ProxyPort = *f.Codex.ProxyPort
		}
		applyBool(&cfg.Codex.CrossAccountReplay, f.Codex.CrossAccountReplay)
	}
	return cfg, nil
}

// validProxyPort holds the pinned port to what the proxy can actually bind,
// and it is checked at LOAD rather than at bind: the symptom of a bad port
// discovered at bind is a daemon that will not start, reported from a layer
// with nothing to say about which key caused it.
//
// 0 is not a port, it is the way to say "resolve one" -- the recorded port, or
// a default derived from the store's own path -- so it is accepted. A
// privileged port is refused because nothing here runs as root and the bind
// would fail; the range is the whole unprivileged port space rather than the
// narrower band the derived default is drawn from, because pinning a port
// outside that band is a legitimate choice a machine may have made for its own
// reasons.
func validProxyPort(v int) error {
	if v == 0 {
		return nil
	}
	if v < 1024 || v > 65535 {
		return fmt.Errorf("%d is not a port ccdad can bind; write 0 to let ccdad resolve one, or a number between 1024 and 65535", v)
	}
	return nil
}

// Load reads the config from disk. A machine with no config file gets the full
// default set and no error — that is the ordinary state, not a degradation.
//
// A file that exists and cannot be read IS an error. Reporting the defaults for
// it would run the engine on numbers the user does not believe are in force,
// and a permission problem on a file they wrote is exactly the case where they
// would never think to look.
func Load() (Config, error) {
	raw, err := readConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Defaults(), nil
		}
		return Config{}, err
	}
	return Parse(raw)
}

// UnknownKeys lists the keys in a document that this ccdad does not know, in
// dotted form and in sorted order.
//
// They are NOT an error. A config written by a newer ccdad has to leave an
// older one running — the same rule the credentials swap follows — so the
// loader ignores them and this reports them, which is how `ccdad config list`
// can say "these are being ignored" instead of the file silently doing less
// than it looks like it does. It is also the only warning a hand-edited typo
// ever gets, since a typo is indistinguishable from a future key.
func UnknownKeys(raw []byte) ([]string, error) {
	d, err := ParseDocument(raw)
	if err != nil {
		return nil, err
	}
	return d.UnknownKeys(), nil
}

// Reloader is the tick loop's "pick up external config changes", with the
// property the cadence alone does not give and the daemon depends on: a broken
// edit must not stop the engine.
//
// Reload always reports what it found, and always hands back a USABLE config —
// the last one that parsed, or the defaults if none ever did. A daemon that
// exits on a bad hand-edit is a daemon that stops switching accounts silently,
// which is worse than running on a config that is one edit out of date.
//
// A Reloader is not safe for concurrent use: it is the tick loop's, and the
// tick loop is one goroutine.
type Reloader struct {
	raw    []byte
	hasRaw bool
	// current is the last config that parsed, or the defaults.
	current Config
	// parseErr belongs to raw and to nothing else. A READ failure is
	// deliberately not kept here: it is transient — a file briefly locked by a
	// backup, a directory momentarily unreadable — and it says nothing about
	// the content, so remembering it would have the daemon reporting a problem
	// that has already gone away for as long as nobody edits the file again.
	parseErr error
}

// NewReloader starts on the defaults, having read nothing.
func NewReloader() *Reloader {
	return &Reloader{current: Defaults()}
}

// Reload re-reads the file and returns the config to run on, plus whatever went
// wrong. The config is usable in every case; the error is a warning to report,
// never a reason to stop.
func (r *Reloader) Reload() (Config, error) {
	raw, err := readConfig()
	if err != nil {
		// An unreadable file is not evidence about its contents, so the last
		// good config stands. It differs from a missing one, which is the user
		// deleting their settings and does return to the defaults.
		if errors.Is(err, os.ErrNotExist) {
			r.raw, r.hasRaw = nil, false
			r.current, r.parseErr = Defaults(), nil
			return r.current, nil
		}
		return r.current, err
	}

	// The change is detected on the BYTES, never on an mtime or a size. Two
	// `ccdad config set` calls inside one filesystem timestamp tick are
	// ordinary, and a heuristic that missed one would leave the daemon running
	// on the old value until something else changed the file.
	if r.hasRaw && bytes.Equal(raw, r.raw) {
		return r.current, r.parseErr
	}
	r.raw, r.hasRaw = append([]byte(nil), raw...), true

	cfg, err := Parse(raw)
	if err != nil {
		r.parseErr = err
		return r.current, err
	}
	// Effective, not the parsed value. Hover forces probe_unknown on, and the
	// daemon reads its whole configuration through this call, so this is the one
	// place that override has to be applied for the probe path to see it.
	r.current, r.parseErr = cfg.Effective(), nil
	return r.current, nil
}

// readConfig reads the raw document, refusing a relative store root.
func readConfig() ([]byte, error) {
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("reading %s: %w", FileName, err)
	}
	return raw, nil
}

// applyFloat overlays one optional float onto its default, validating it first.
func applyFloat(dst *float64, v *float64, key string, valid func(float64) error) error {
	if v == nil {
		return nil
	}
	if err := valid(*v); err != nil {
		return fmt.Errorf("%s in %s: %w", key, FileName, err)
	}
	*dst = *v
	return nil
}

// parseConfigDuration is the form every duration key takes, and the one
// sentence that explains it. A bare number is refused rather than guessed at:
// TOML has no duration type and `cooldown = 300` is as readable as seconds as
// it is as nanoseconds.
func parseConfigDuration(key string, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s in %s: %q is not a duration; write it as a Go duration string such as \"5m\" or \"300s\"", key, FileName, v)
	}
	return d, nil
}

// applyPreemptLead overlays preempt_lead, the one duration key whose ZERO is a
// value rather than a mechanism switched off.
//
// The pre-emptive switch is an opt-out: a user may decide they would rather
// never be moved off an account early, and 0 is how they say so. A cooldown or
// a hysteresis has no such setting, which is why applyDuration refuses a zero
// and this does not. A NEGATIVE lead is refused by both: it would put the
// switch after the exhaustion it exists to get ahead of, which is not a shorter
// lead but a nonsense one.
func applyPreemptLead(dst *time.Duration, v *string) error {
	const key = keyPreemptLead
	if v == nil {
		return nil
	}
	d, err := parseConfigDuration(key, *v)
	if err != nil {
		return err
	}
	if d < 0 {
		return fmt.Errorf("%s in %s: %q is negative, and a lead that runs backwards would place the switch after the exhaustion it exists to get ahead of", key, FileName, *v)
	}
	*dst = d
	return nil
}

// applyBool overlays one optional bool.
//
// It takes no validator and returns no error, unlike its float and duration
// siblings: a bool has exactly two legal values, and the only way to write
// anything else is a type go-toml refuses before Parse is ever called —
// `hover = "true"` fails at Unmarshal the same way a quoted threshold does.
func applyBool(dst *bool, v *bool) {
	if v == nil {
		return
	}
	*dst = *v
}

// applyString overlays one optional string onto its default, validating it
// first. It is applyFloat's shape for the keys whose values are NAMES.
//
// It does not trim, and that is the difference from coerceName on the writing
// side. A value that came through a shell can carry a space nobody typed, so
// `ccdad config set` trims; a space in the FILE is a hand edit that meant
// something, and repairing it silently would have the same document read one
// way through the loader and another way through the CLI.
func applyString(dst *string, v *string, key string, valid func(string) error) error {
	if v == nil {
		return nil
	}
	if err := valid(*v); err != nil {
		return fmt.Errorf("%s in %s: %w", key, FileName, err)
	}
	*dst = *v
	return nil
}

// applyWindowThresholds overlays the [window_threshold] table.
//
// Every value is validated exactly as `threshold` is, and that is not a
// formality: go-toml decodes `five_hour = inf` into a float64 without
// complaint, and an infinite threshold puts that window infinitely far from its
// own floor, so the one window the user tightened becomes the one that can
// never bind.
//
// The NAMES are not refused here. A key this build does not recognize has to
// round-trip — a config written by a newer ccdad must leave an older one
// running — so an unrecognized name is carried and simply never looked up,
// which is the same answer the loader gives an unknown top-level key. Refusing
// a name at the point a HUMAN types it is a different question with a different
// answer, and it belongs where `ccdad config set` lives.
//
// Keys are visited in sorted order so a file with two bad values names the same
// one on every run. Map iteration order would make the same command print a
// different error each time it was run.
func applyWindowThresholds(cfg *Config, table map[string]float64) error {
	if len(table) == 0 {
		return nil
	}
	out := make(map[usage.WindowName]float64, len(table))
	for _, name := range slices.Sorted(maps.Keys(table)) {
		v := table[name]
		if err := validThreshold(v); err != nil {
			return fmt.Errorf(windowThresholdPrefix+"%s in %s: %w", name, FileName, err)
		}
		out[usage.WindowName(name)] = v
	}
	cfg.WindowThreshold = out
	return nil
}

// applyDuration keeps its own non-positive refusal, and the sentence is exact:
// none of the keys that reach it has a disabled setting. A zero cooldown is a
// switch storm and a zero hysteresis is no margin at all.
func applyDuration(dst *time.Duration, v *string, key string) error {
	if v == nil {
		return nil
	}
	d, err := parseConfigDuration(key, *v)
	if err != nil {
		return err
	}
	if d <= 0 {
		return fmt.Errorf("%s in %s: %q is not positive, and an anti-flap mechanism cannot be switched off", key, FileName, *v)
	}
	*dst = d
	return nil
}

func parseStrategy(name string) (strategy.Strategy, error) {
	s, ok := strategy.ParseStrategy(name)
	if !ok {
		return 0, fmt.Errorf("%s in %s: unknown strategy %q: one of %s",
			keyStrategy, FileName, name, joinNames(strategy.StrategyNames()))
	}
	return s, nil
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// finite refuses the two values that defeat a range check rather than failing
// it. The credit gate spells this out for max_auto_spend, and the loader holds
// every float key to it: a NaN threshold makes `100-pct > threshold` false for
// every account, so nothing is ever over threshold and the recovery mode
// becomes unreachable.
func finite(v float64) error {
	switch {
	case math.IsNaN(v):
		return errors.New("not a number, so every comparison against it would answer false")
	case math.IsInf(v, 0):
		return errors.New("infinite, which is never a usable bound")
	}
	return nil
}

func validThreshold(v float64) error {
	if err := finite(v); err != nil {
		return err
	}
	if v <= 0 || v > 100 {
		return fmt.Errorf("%v is not a utilization percent: it must be greater than 0 and at most 100", v)
	}
	return nil
}

func validHysteresisPct(v float64) error {
	if err := finite(v); err != nil {
		return err
	}
	if v <= 0 || v > 100 {
		return fmt.Errorf("%v is not a margin in points of headroom: it must be greater than 0 and at most 100", v)
	}
	return nil
}

func validHeadroomRatio(v float64) error {
	if err := finite(v); err != nil {
		return err
	}
	// Below 1.0 a candidate with LESS headroom than the live account clears the
	// "margin", which inverts the anti-flap mechanism instead of loosening it.
	// 1.0 is the honest way to say "no multiplicative margin".
	if v < 1 {
		return fmt.Errorf("%v is below 1.0, which would let an account with less headroom displace the live one", v)
	}
	return nil
}

func validMaxAutoSpend(v float64) error {
	if err := finite(v); err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("%v is negative", v)
	}
	return nil
}
