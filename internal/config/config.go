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
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// FileName is the config file's basename inside the ccdad store.
const FileName = "config.toml"

// Config is the effective configuration: what the file says, over the defaults.
//
// It is a plain comparable struct with no pointers on purpose — absence is
// resolved during parsing and never leaks out of this package, so no consumer
// has to ask whether a field was set.
type Config struct {
	// Threshold is the utilization percent above which an account counts as
	// spent.
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
// decided.
func (c Config) RankOptions(now time.Time) strategy.Options {
	return strategy.Options{Now: now, Threshold: c.Threshold, Strategy: c.Strategy}
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
	Threshold          *float64    `toml:"threshold"`
	HysteresisPct      *float64    `toml:"hysteresis_pct"`
	HeadroomRatio      *float64    `toml:"headroom_ratio"`
	Cooldown           *string     `toml:"cooldown"`
	RecoveryHysteresis *string     `toml:"recovery_hysteresis"`
	Strategy           *string     `toml:"strategy"`
	Credit             *creditFile `toml:"credit"`
}

type creditFile struct {
	MaxAutoSpend *float64 `toml:"max_auto_spend"`
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
	if f.Strategy != nil {
		s, err := parseStrategy(*f.Strategy)
		if err != nil {
			return Config{}, err
		}
		cfg.Strategy = s
	}
	if f.Credit != nil && f.Credit.MaxAutoSpend != nil {
		if err := applyFloat(&cfg.MaxAutoSpend, f.Credit.MaxAutoSpend, keyMaxAutoSpend, validMaxAutoSpend); err != nil {
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
	r.current, r.parseErr = cfg, nil
	return cfg, nil
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

// applyDuration overlays one optional duration, which is written as a Go
// duration string ("5m", "300s").
//
// A bare number is refused rather than guessed at: TOML has no duration type,
// `cooldown = 300` is as readable as seconds as it is as nanoseconds, and Go's
// own time.Duration prints in the string form, so a value that round-trips
// through `ccdad config get` reads the same as one a user typed.
func applyDuration(dst *time.Duration, v *string, key string) error {
	if v == nil {
		return nil
	}
	d, err := time.ParseDuration(*v)
	if err != nil {
		return fmt.Errorf("%s in %s: %q is not a duration; write it as a Go duration string such as \"5m\" or \"300s\"", key, FileName, *v)
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
