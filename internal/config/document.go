package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// The writable half of config.toml, for `ccdad config set|unset`.
//
// A Document is the file as WRITTEN — a generic table, not the typed struct the
// loader produces — because a set has to round-trip what it does not
// understand. Decoding into fileShape and re-marshalling would silently delete
// a key a newer ccdad wrote, which is the failure the credentials swap's
// round-trip rule exists against and is the same failure here.

const (
	// configLockDir is a DIRECTORY, because that is what cclock's mutex is.
	//
	// WHY cclock AND NOT the store's flock. Each file in the store has exactly
	// one lock owner and this is a new file, so the choice is free: usage.json
	// is cclock's, accounts.toml is flock's. cclock wins here because it is the
	// exported one — internal/store's flock helpers are unexported, and copying
	// the try-lock seam, the error classification and the wait loop into this
	// package to save a five-line call would be a third copy of a primitive
	// this binary already ships twice.
	//
	// cclock's one weakness does not reach this file: its staleness heuristic
	// can steal a lock a live holder still owns, which matters for a lock held
	// across a daemon tick. Nothing holds this one across anything — the daemon
	// only READS config.toml, and reads take no lock at all, so every holder is
	// an interactive `ccdad config` command finishing in milliseconds.
	configLockDir = FileName + ".lock"

	// configLockStale mirrors the usage cache's, and for the same reason: a
	// config write is a sub-second operation, so this only matters after a
	// crash, and it must stay at least twice cclock's touch interval or a live
	// holder's lock goes stale by its own definition between two touches.
	configLockStale = 30 * time.Second
)

// LockTimeout bounds how long `ccdad config set` waits for the lock. It is a
// var so a test can shrink it and reach the timeout path without an unbounded
// contention test.
var LockTimeout = 5 * time.Second

// ErrUnknownKey is a key this release does not have. It is a sentinel because
// the CLI maps it to exit 2 — a typo is a usage error — while a bad VALUE for
// a real key is a different sentence with the same exit code, and `ccdad
// doctor` may one day want to tell them apart.
var ErrUnknownKey = errors.New("unknown config key")

// Document is config.toml as it is written.
type Document struct {
	raw map[string]any
}

func newDocument() *Document { return &Document{raw: map[string]any{}} }

// ParseDocument reads a document without touching the filesystem.
func ParseDocument(raw []byte) (*Document, error) {
	doc := map[string]any{}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", FileName, err)
	}
	return &Document{raw: doc}, nil
}

// LoadDocument reads the document from disk, or an empty one when there is no
// file yet. It takes no lock: every write is an atomic rename, so a reader sees
// one whole version of the document or another.
func LoadDocument() (*Document, error) {
	raw, err := readConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newDocument(), nil
		}
		return nil, err
	}
	return ParseDocument(raw)
}

// Config is the effective configuration this document describes.
func (d *Document) Config() (Config, error) {
	encoded, err := d.Encode()
	if err != nil {
		return Config{}, err
	}
	return Parse(encoded)
}

// Encode renders the document back to TOML.
func (d *Document) Encode() ([]byte, error) {
	out, err := toml.Marshal(d.raw)
	if err != nil {
		return nil, fmt.Errorf("encoding %s: %w", FileName, err)
	}
	return out, nil
}

// UnknownKeys lists the keys this release does not know AND does not read, in
// dotted sorted form. Its whole use is a notice telling a reader those keys are
// being ignored, so a key that is in fact read may not appear here — see
// OptedInWindows for the one kind that is.
func (d *Document) UnknownKeys() []string {
	out := []string{}
	for name, value := range d.raw {
		if table, isTable := value.(map[string]any); isTable && isKnownSection(name) {
			for sub := range table {
				key := name + "." + sub
				if !isKnownKey(key) && !optedInWindow(key) {
					out = append(out, key)
				}
			}
			continue
		}
		if !isKnownKey(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// OptedInWindows lists the window_threshold keys this release cannot validate
// and reads anyway, in dotted sorted form.
//
// They are the exception to everything UnknownKeys says. A weekly cap filed
// under a scope key this build does not name is carried but left OUT of the
// ranking, because ccdad cannot state what such a cap covers; a threshold naming
// that window is the opt-in, and the only one, since `ccdad config set` refuses
// a scope it cannot verify. So the key is unknown to the config surface and live
// to the engine at the same time, and reporting it as ignored tells a user who
// just typed it that it does nothing.
func (d *Document) OptedInWindows() []string {
	table, isTable := d.raw[windowThresholdSection].(map[string]any)
	if !isTable {
		return nil
	}
	out := []string{}
	for name := range table {
		if key := windowThresholdPrefix + name; optedInWindow(key) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// optedInWindow reports whether a key names a window this release cannot
// validate but the ranking still reads.
//
// It asks usage.ValidWindowName for the REASON rather than going through
// windowOf, which answers a bool: three of that function's four refusals mean
// the entry is dead — a name no reading produces, cinder_cove, a scoped name
// with no display half — and only ErrUnknownScope means "not yet". Widening
// windowOf to carry the reason would push that distinction onto every caller
// that only wants the gate.
func optedInWindow(key string) bool {
	name, inSection := strings.CutPrefix(key, windowThresholdPrefix)
	if !inSection {
		return false
	}
	return errors.Is(usage.ValidWindowName(usage.WindowName(name)), usage.ErrUnknownScope)
}

// Keys is every key `ccdad config list` prints for THIS document: the ones
// Keys() names, and the window thresholds the document itself carries.
//
// The second half cannot come from a list. A scoped window is named after a
// model or a surface the server invented, so the set of legal names is open and
// only the file knows which of them are in use. A document that names no window
// therefore adds no row at all rather than a placeholder row for a key nothing
// has set — which would be a row `ccdad config unset` could not remove.
//
// A name in the table that is not a window is left out here and named in a
// notice instead. For most of them that notice is UnknownKeys', the answer an
// unknown top-level key already gets: it round-trips, it is ignored, and it is
// named once. A scoped name under a key this build does not know is the
// exception and gets its own, because that one IS read — see OptedInWindows. That omission is
// load-bearing rather than tidy — `ccdad config list` calls Config.Value on
// every key it is handed and returns the error, so offering one would turn a
// forward-compatible file into a failed command.
func (d *Document) Keys() []string {
	keys := Keys()
	table, isTable := d.raw[windowThresholdSection].(map[string]any)
	if !isTable {
		return keys
	}
	windows := make([]string, 0, len(table))
	for name := range table {
		if key := windowThresholdPrefix + name; isKnownKey(key) {
			windows = append(windows, key)
		}
	}
	// Sorted, because a map has no order: two runs of `ccdad config list` over
	// one unchanged file would otherwise print its rows in two different
	// orders.
	sort.Strings(windows)
	// Keys() hands back a fresh slice on every call, so appending to it cannot
	// alias the fixed list.
	return append(keys, windows...)
}

// Value is the key's value AS THE FILE HOLDS IT, and whether the file holds one
// at all. `ccdad config get` answers exit 5 on the false, which the exit
// contract reserves for a negative answer to a probe rather than a failure.
func (d *Document) Value(key string) (string, bool, error) {
	if !isKnownKey(key) {
		return "", false, unknownKey(key)
	}
	table, name := d.locate(key, false)
	if table == nil {
		return "", false, nil
	}
	v, ok := table[name]
	if !ok {
		return "", false, nil
	}
	return format(v), true, nil
}

// Set validates a value the way the loader would and stores it.
//
// Validation happens HERE and not only at read time, because the alternative is
// a `config set` that succeeds and a daemon that then refuses the whole file:
// the user would have written a value nothing rejected and switched nothing off
// but the engine.
func (d *Document) Set(key, value string) error {
	if !isKnownKey(key) {
		return unknownKey(key)
	}
	stored, err := coerce(key, value)
	if err != nil {
		return err
	}
	table, name := d.locate(key, true)
	table[name] = stored
	return nil
}

// Unset removes the key and reports whether it was there. It is not an error
// for it to be absent — that is exit 3, the world already being as the caller
// asked.
func (d *Document) Unset(key string) (bool, error) {
	if !isKnownKey(key) {
		return false, unknownKey(key)
	}
	table, name := d.locate(key, false)
	if table == nil {
		return false, nil
	}
	if _, ok := table[name]; !ok {
		return false, nil
	}
	delete(table, name)
	// A section emptied by the removal goes too, so unsetting the only key in
	// [credit] does not leave a bare header behind. A section that still holds
	// anything — including a key this release does not know — stays.
	if section, _, nested := strings.Cut(key, "."); nested && len(table) == 0 {
		delete(d.raw, section)
	}
	return true, nil
}

// locate finds the table a dotted key lives in and its leaf name. With create
// false it returns a nil table when the section is absent, so a read never
// materializes one.
func (d *Document) locate(key string, create bool) (map[string]any, string) {
	section, leaf, nested := strings.Cut(key, ".")
	if !nested {
		return d.raw, key
	}
	existing, ok := d.raw[section]
	if ok {
		if table, isTable := existing.(map[string]any); isTable {
			return table, leaf
		}
		// The section name is occupied by something that is not a table — a
		// hand-edited `credit = 5`. A read finds nothing; a write replaces it,
		// because the alternative is a config file no `ccdad config` command
		// can ever repair.
	}
	if !create {
		return nil, leaf
	}
	table := map[string]any{}
	d.raw[section] = table
	return table, leaf
}

// coerce turns a command-line string into the value the file should carry,
// applying the same validation the loader applies to what it reads.
func coerce(key, value string) (any, error) {
	// A per-window threshold is held to the same bounds as the top-level one it
	// falls back to, so no window can be given a number the loader would refuse
	// for `threshold` itself.
	if _, ok := windowOf(key); ok {
		return coerceFloat(key, value, validThreshold)
	}
	switch key {
	case keyThreshold, keyCreditThreshold, keyCodexThreshold:
		return coerceFloat(key, value, validThreshold)
	case keyCodexBinary:
		// Stored as typed, with no name list to check against: it is a path,
		// and this package cannot tell a path that does not exist yet from one
		// that is wrong. The launcher reports a binary it cannot run.
		return strings.TrimSpace(value), nil
	case keyCodexProxyPort:
		return coerceInt(key, value, validProxyPort)
	case keyHysteresisPct:
		return coerceFloat(key, value, validHysteresisPct)
	case keyHeadroomRatio:
		return coerceFloat(key, value, validHeadroomRatio)
	case keyMaxAutoSpend:
		return coerceFloat(key, value, validMaxAutoSpend)
	case keyCooldown, keyRecoveryHysteresis:
		var d time.Duration
		if err := applyDuration(&d, &value, key); err != nil {
			return nil, err
		}
		// Stored canonically, so `config get` reads back what `config set`
		// meant rather than what it was typed as.
		return d.String(), nil
	case keyPreemptLead:
		// A duration like the two above, and NOT the same rule: zero is how
		// the pre-emptive switch is turned off, while a zero cooldown is an
		// anti-flap mechanism disabled. So it runs the overlay Parse runs for
		// this key rather than the one Parse runs for those — a `config set`
		// stricter than the loader would refuse the one value that documents
		// the feature's own off switch and leave a hand edit as the only way
		// to write it.
		var d time.Duration
		if err := applyPreemptLead(&d, &value); err != nil {
			return nil, err
		}
		return d.String(), nil
	case keyStrategy:
		s, err := parseStrategy(value)
		if err != nil {
			return nil, err
		}
		return s.String(), nil
	case keyProbeUnknown, keyHover, keyManual, keyMCPSwitchWithoutElicitation, keyUpdateCheck,
		keyCodexCrossAccountReplay:
		return coerceBool(key, value)
	case keyTUITheme:
		return coerceName(key, value, validTheme)
	case keyTUIGlyphs:
		return coerceName(key, value, validGlyphs)
	}
	return nil, unknownKey(key)
}

// coerceBool stores a real boolean rather than the string that was typed, so
// the file always reads `hover = true` whichever spelling was given.
//
// `yes` and `on` are not among the spellings ParseBool takes, and that is the
// right answer here rather than a gap: TOML has no such literal, so accepting
// one would write a file the loader then refuses wholesale.
func coerceBool(key, value string) (any, error) {
	b, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a boolean; write true or false", key, value)
	}
	return b, nil
}

// coerceName stores one of a fixed set of words, trimmed.
//
// It trims where the loader does not, and the asymmetry is the point rather
// than an oversight: this value arrived through a shell, which can attach a
// space nobody typed -- `ccdad config set tui.theme " dark"` out of a variable
// that had one -- while a space in the file is a hand edit, and a hand edit
// that says something unusable is worth reporting rather than repairing behind
// the user's back.
func coerceName(key, value string, valid func(string) error) (any, error) {
	name := strings.TrimSpace(value)
	if err := valid(name); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return name, nil
}

func coerceFloat(key, value string, valid func(float64) error) (any, error) {
	// ParseFloat accepts "inf" and "nan", which is exactly why valid() runs
	// after it rather than instead of it.
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a number", key, value)
	}
	if err := valid(f); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

// coerceInt stores a real integer rather than the string that was typed, so
// the file always reads `proxy_port = 24680`. It is separate from coerceFloat
// because a port written as 24680.0 is a mistake rather than a spelling.
func coerceInt(key, value string, valid func(int) error) (any, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a whole number", key, value)
	}
	if err := valid(n); err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return int64(n), nil
}

// format renders a stored value the way `ccdad config get` prints it.
//
// It handles the integer and boolean cases because the file is hand-editable:
// `threshold = 90` decodes as an int64 and must not print as "90.000000" or as
// a Go value dump.
func format(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	}
	return fmt.Sprint(v)
}

// Value is the EFFECTIVE value of a key: what the file says, or the default.
// `ccdad config list` prints these, so every key has an answer whether or not
// the file mentions it.
func (c Config) Value(key string) (string, error) {
	if window, ok := windowOf(key); ok {
		if v, set := c.WindowThreshold[window]; set {
			return format(v), nil
		}
		// The per-window default IS `threshold`: a window with no key of its
		// own is ranked against the top-level number, so reporting anything
		// else here would name a value the engine never uses. Reading the nil
		// map above is the ordinary state and is safe.
		return format(c.Threshold), nil
	}
	switch key {
	case keyThreshold:
		return format(c.Threshold), nil
	case keyHysteresisPct:
		return format(c.HysteresisPct), nil
	case keyHeadroomRatio:
		return format(c.HeadroomRatio), nil
	case keyCooldown:
		return c.Cooldown.String(), nil
	case keyRecoveryHysteresis:
		return c.RecoveryHysteresis.String(), nil
	case keyPreemptLead:
		return c.PreemptLead.String(), nil
	case keyStrategy:
		return c.Strategy.String(), nil
	case keyProbeUnknown:
		return format(c.ProbeUnknown), nil
	case keyHover:
		return format(c.Hover), nil
	case keyManual:
		return format(c.Manual), nil
	case keyMCPSwitchWithoutElicitation:
		return format(c.MCPSwitchWithoutElicitation), nil
	case keyUpdateCheck:
		return format(c.UpdateCheck), nil
	case keyTUITheme:
		return c.TUITheme, nil
	case keyTUIGlyphs:
		return c.TUIGlyphs, nil
	case keyCreditThreshold:
		return format(c.CreditThreshold), nil
	case keyMaxAutoSpend:
		return format(c.MaxAutoSpend), nil
	case keyCodexThreshold:
		return format(c.Codex.Threshold), nil
	case keyCodexBinary:
		return c.Codex.Binary, nil
	case keyCodexProxyPort:
		return format(int64(c.Codex.ProxyPort)), nil
	case keyCodexCrossAccountReplay:
		return format(c.Codex.CrossAccountReplay), nil
	}
	return "", unknownKey(key)
}

func unknownKey(key string) error {
	if name, inSection := strings.CutPrefix(key, windowThresholdPrefix); inSection {
		// The list of top-level keys is no help for this one: the key is in the
		// right table already and it is the WINDOW that is not one. The whole
		// sentence comes from the validator, which renders it from the same two
		// tables it checks against — so the names offered here cannot drift
		// from the names a threshold may actually be set on, and cinder_cove
		// keeps its own sentence about being an expiry rather than being told
		// it is misspelled.
		//
		// The err != nil guard is what makes a well-formed window name that
		// reaches here anyway fall through to the list of top-level keys: a
		// nil wrapped by %w prints %!w(<nil>) and would leave the refusal
		// saying nothing at all.
		if err := usage.ValidWindowName(usage.WindowName(name)); err != nil {
			return fmt.Errorf("%w %q: %w", ErrUnknownKey, key, err)
		}
	}
	return fmt.Errorf("%w %q: one of %s", ErrUnknownKey, key, joinNames(Keys()))
}

// WithDocument runs fn against the document under a cross-process lock and
// writes back what it changed.
//
// The read happens INSIDE the lock, which is the half an atomic rename cannot
// provide: two `ccdad config set` calls that each read the file, change one key
// and write it back would otherwise lose one of the two keys, because the
// second rename replaces a document the first one's reader never saw.
//
// fn returning an error leaves the file exactly as it was.
func WithDocument(fn func(*Document) error) (err error) {
	root, rerr := storeRoot()
	if rerr != nil {
		return rerr
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("creating the ccdad store: %w", err)
	}

	lock, aerr := cclock.Acquire(filepath.Join(root, configLockDir), cclock.Options{
		Stale:   configLockStale,
		Timeout: LockTimeout,
	})
	if aerr != nil {
		return fmt.Errorf("locking %s: %w", FileName, aerr)
	}
	// Release's return value is part of the answer, not noise: the synchronous
	// re-stat it performs is the only check that can see a takeover in the
	// window between the touch goroutine's last tick and now, and discarding it
	// would report success for exactly the write that raced.
	defer func() { err = errors.Join(err, lock.Release()) }()

	d, err := LoadDocument()
	if err != nil {
		return err
	}
	if err := fn(d); err != nil {
		return err
	}
	encoded, err := d.Encode()
	if err != nil {
		return err
	}
	// The same atomic rename every other ccdad document is written with: the
	// daemon's tick loop re-reads this file every second, and a reader must
	// never catch a half-written config.
	return cclink.WriteFileAtomic(filepath.Join(root, FileName), encoded, 0o600)
}
