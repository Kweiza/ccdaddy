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
	Threshold          *float64           `toml:"threshold"`
	HysteresisPct      *float64           `toml:"hysteresis_pct"`
	HeadroomRatio      *float64           `toml:"headroom_ratio"`
	Cooldown           *string            `toml:"cooldown"`
	RecoveryHysteresis *string            `toml:"recovery_hysteresis"`
	PreemptLead        *string            `toml:"preempt_lead"`
	Strategy           *string            `toml:"strategy"`
	ProbeUnknown       *bool              `toml:"probe_unknown"`
	Hover              *bool              `toml:"hover"`
	WindowThreshold    map[string]float64 `toml:"window_threshold"`
	Credit             *creditFile        `toml:"credit"`
}

type creditFile struct {
	MaxAutoSpend *float64 `toml:"max_auto_spend"`
	Threshold    *float64 `toml:"threshold"`
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
	return cfg, nil
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
