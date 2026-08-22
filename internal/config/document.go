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
)

// The writable half of config.toml, for `ccdad config set|unset`.
//
// A Document is the file as WRITTEN — a generic table, not the typed struct the
// loader produces — because a set has to round-trip what it does not
// understand. Decoding into fileShape and re-marshalling would silently delete
// a key a newer ccdad wrote, which is the failure §4.2 rule 3 names for the
// credentials file and is the same failure here.

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
// the CLI maps it to §9.3's exit 2 — a typo is a usage error — while a bad
// VALUE for a real key is a different sentence with the same exit code, and
// `ccdad doctor` may one day want to tell them apart.
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

// UnknownKeys lists the keys this release does not know, in dotted sorted form.
func (d *Document) UnknownKeys() []string {
	out := []string{}
	for name, value := range d.raw {
		if table, isTable := value.(map[string]any); isTable && isKnownSection(name) {
			for sub := range table {
				if !isKnownKey(name + "." + sub) {
					out = append(out, name+"."+sub)
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

// Value is the key's value AS THE FILE HOLDS IT, and whether the file holds one
// at all. `ccdad config get` answers exit 5 on the false, which is §9.3's
// negative answer to a probe rather than a failure.
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
// for it to be absent — that is §9.3's exit 3, the world already being as the
// caller asked.
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
	switch key {
	case keyThreshold:
		return coerceFloat(key, value, validThreshold)
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
	case keyStrategy:
		s, err := parseStrategy(value)
		if err != nil {
			return nil, err
		}
		return s.String(), nil
	}
	return nil, unknownKey(key)
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
	case keyStrategy:
		return c.Strategy.String(), nil
	case keyMaxAutoSpend:
		return format(c.MaxAutoSpend), nil
	}
	return "", unknownKey(key)
}

func unknownKey(key string) error {
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
	// daemon re-reads this file on every tick (§8.4), and a reader must never
	// catch a half-written config.
	return cclink.WriteFileAtomic(filepath.Join(root, FileName), encoded, 0o600)
}
