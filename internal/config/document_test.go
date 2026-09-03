package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func TestAnIntegerLiteralIsAcceptedForAFloatKey(t *testing.T) {
	// `threshold = 90` is what a person types. TOML calls it an integer, and a
	// loader that refused it would be technically right and unusable.
	cfg, err := Parse([]byte("threshold = 90\n"))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if cfg.Threshold != 90 {
		t.Errorf("Threshold = %v, want 90", cfg.Threshold)
	}
}

func TestADocumentWithNoFileIsTheDefaults(t *testing.T) {
	write(t, "")

	d, err := LoadDocument()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := d.Config()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Equal(Defaults()) {
		t.Errorf("Config() = %+v, want the defaults", cfg)
	}
	if _, set, err := d.Value(keyThreshold); err != nil || set {
		t.Errorf("Value(threshold) = set %v, err %v; nothing is set in an empty document", set, err)
	}
}

func TestSettingAnUnknownKeyIsRefusedByName(t *testing.T) {
	d := newDocument()
	err := d.Set("threshhold", "90")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Set(typo) = %v, want ErrUnknownKey; an accepted typo is a config that silently does nothing", err)
	}
	if !strings.Contains(err.Error(), keyThreshold) {
		t.Errorf("error = %q, want it to list the keys that do exist", err)
	}
}

func TestSettingAnInvalidValueIsRefusedAndIsNotAnUnknownKey(t *testing.T) {
	d := newDocument()
	err := d.Set(keyMaxAutoSpend, "inf")
	if err == nil {
		t.Fatal("set credit.max_auto_spend inf was accepted")
	}
	if errors.Is(err, ErrUnknownKey) {
		t.Errorf("error = %v, want a value error rather than an unknown-key one", err)
	}
	if _, set, _ := d.Value(keyMaxAutoSpend); set {
		t.Error("a refused Set still wrote the value")
	}
}

func TestADurationIsStoredInItsCanonicalForm(t *testing.T) {
	d := newDocument()
	if err := d.Set(keyCooldown, "300s"); err != nil {
		t.Fatal(err)
	}
	got, set, err := d.Value(keyCooldown)
	if err != nil || !set {
		t.Fatalf("Value() = %q, set %v, err %v", got, set, err)
	}
	if got != "5m0s" {
		t.Errorf("Value() = %q, want the canonical 5m0s so that get and set agree", got)
	}
	cfg, err := d.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cooldown != 5*time.Minute {
		t.Errorf("Cooldown = %v, want 5m", cfg.Cooldown)
	}
}

func TestANestedKeyLandsInItsOwnTable(t *testing.T) {
	d := newDocument()
	if err := d.Set(keyMaxAutoSpend, "25"); err != nil {
		t.Fatal(err)
	}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "[credit]") {
		t.Errorf("encoded document has no [credit] table:\n%s", encoded)
	}
	cfg, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxAutoSpend != 25 {
		t.Errorf("MaxAutoSpend = %v, want 25", cfg.MaxAutoSpend)
	}
}

// The round-trip rule, applied to this file: decoding into a typed struct and
// re-marshalling would silently delete a key a newer ccdad wrote.
func TestSettingOneKeyPreservesEverythingTheDocumentAlreadyHeld(t *testing.T) {
	d, err := ParseDocument([]byte("threshold = 70\nfuture_knob = 3\n\n[future]\nkeep = \"me\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Set(keyHysteresisPct, "12"); err != nil {
		t.Fatal(err)
	}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"future_knob", "[future]", "keep", "threshold"} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("re-encoded document dropped %q:\n%s", want, encoded)
		}
	}
}

func TestUnsetReportsWhetherThereWasAnythingToRemove(t *testing.T) {
	d, err := ParseDocument([]byte("threshold = 70\n"))
	if err != nil {
		t.Fatal(err)
	}
	removed, err := d.Unset(keyThreshold)
	if err != nil || !removed {
		t.Fatalf("Unset() = %v, %v; want true", removed, err)
	}
	removed, err = d.Unset(keyThreshold)
	if err != nil || removed {
		t.Fatalf("Unset() on an already-unset key = %v, %v; want false so the CLI can answer exit 3", removed, err)
	}
	cfg, err := d.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Threshold != Defaults().Threshold {
		t.Errorf("Threshold = %v, want the default back", cfg.Threshold)
	}
}

func TestUnsettingAnUnknownKeyIsRefused(t *testing.T) {
	d := newDocument()
	if _, err := d.Unset("threshhold"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Unset(typo) = %v, want ErrUnknownKey", err)
	}
}

func TestUnsettingTheLastKeyOfASectionLeavesTheDocumentParseable(t *testing.T) {
	d, err := ParseDocument([]byte("[credit]\nmax_auto_spend = 25\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Unset(keyMaxAutoSpend); err != nil {
		t.Fatal(err)
	}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(encoded)
	if err != nil {
		t.Fatalf("re-parsing after the last key of a section was removed: %v\n%s", err, encoded)
	}
	if cfg.MaxAutoSpend != 0 {
		t.Errorf("MaxAutoSpend = %v, want 0", cfg.MaxAutoSpend)
	}
	if strings.Contains(string(encoded), "[credit]") {
		t.Errorf("an empty [credit] header was left behind:\n%s", encoded)
	}
}

func TestWithDocumentWritesTheFileAtomicallyAndPrivately(t *testing.T) {
	home := write(t, "")

	if err := WithDocument(func(d *Document) error { return d.Set(keyThreshold, "55") }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, FileName))
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no mode bits: os.Chmod there toggles the read-only attribute
	// and Stat reports 0666 whatever the file was created with. v1 relies on
	// the inherited %USERPROFILE% ACL instead -- which is a property of the
	// directory, not of this file, and not something a Go test can assert
	// here.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %v, want 0600", perm)
		}
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Threshold != 55 {
		t.Errorf("Threshold = %v, want the written 55", cfg.Threshold)
	}
}

func TestWithDocumentSeesWhatIsOnDiskWhenItRuns(t *testing.T) {
	home := write(t, "threshold = 70\n")
	// The read-modify-write happens INSIDE the lock, so a callback must be
	// looking at the file as it is now rather than at a snapshot taken before.
	if err := os.WriteFile(filepath.Join(home, FileName), []byte("threshold = 33\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WithDocument(func(d *Document) error {
		got, set, err := d.Value(keyThreshold)
		if err != nil {
			return err
		}
		if !set || got != "33" {
			t.Errorf("Value(threshold) = %q, set %v; want the 33 that is on disk", got, set)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAFailedCallbackLeavesTheFileExactlyAsItWas(t *testing.T) {
	home := write(t, "threshold = 70\n")
	boom := errors.New("boom")

	err := WithDocument(func(d *Document) error {
		if err := d.Set(keyThreshold, "12"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithDocument() = %v, want the callback's error", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "threshold = 70\n" {
		t.Errorf("file = %q, want it untouched", raw)
	}
}

func TestEveryKeyCanBeSetAndReadBack(t *testing.T) {
	// probe_unknown and hover are set to the OPPOSITE of their defaults on
	// purpose: a boolean set to the value it already had would pass this test
	// without the set having landed anywhere.
	values := map[string]string{
		keyThreshold:          "55",
		keyHysteresisPct:      "12.5",
		keyHeadroomRatio:      "3",
		keyCooldown:           "7m0s",
		keyRecoveryHysteresis: "1m30s",
		keyPreemptLead:        "3m0s",
		keyStrategy:           "consume-first",
		keyProbeUnknown:       "false",
		keyManual:             "true",
		keyHover:              "true",
		keyCreditThreshold:    "70",
		keyMaxAutoSpend:       "25",

		keyMCPSwitchWithoutElicitation: "true",
		keyUpdateCheck:                 "false",

		keyCodexThreshold:          "65",
		keyCodexBinary:             "/opt/codex/bin/codex",
		keyCodexProxyPort:          "24680",
		keyCodexCrossAccountReplay: "true",

		// Neither is the default, for the reason the booleans above are not: a
		// key set to the value it already had would pass this test without the
		// set having landed anywhere.
		keyTUITheme:  "dark",
		keyTUIGlyphs: "ascii",
	}
	d := newDocument()
	for _, k := range Keys() {
		v, named := values[k]
		if !named {
			// A key added to keys.go and not to this table would otherwise
			// fail on an empty value, which reads as a broken coercion rather
			// than as the missing line it is.
			t.Fatalf("Keys() names %q and this test gives it no value; a key added to the list is added here too", k)
		}
		if err := d.Set(k, v); err != nil {
			t.Fatalf("Set(%s) = %v", k, err)
		}
	}
	// The open half of the namespace, which Keys() cannot name: a per-window
	// threshold is set and read back under the same dotted key.
	if err := d.Set("window_threshold.five_hour", "85"); err != nil {
		t.Fatalf("Set(window_threshold.five_hour) = %v", err)
	}
	values["window_threshold.five_hour"] = "85"
	for _, k := range d.Keys() {
		got, set, err := d.Value(k)
		if err != nil || !set {
			t.Fatalf("Value(%s) = %q, set %v, err %v", k, got, set, err)
		}
		if got != values[k] {
			t.Errorf("Value(%s) = %q, want %q", k, got, values[k])
		}
	}
	cfg, err := d.Config()
	if err != nil {
		t.Fatal(err)
	}
	// Every key in the list was set, so nothing below is a default: a field
	// this literal does not name is one the loop above did not cover.
	want := Config{
		Threshold:          55,
		HysteresisPct:      12.5,
		HeadroomRatio:      3,
		Cooldown:           7 * time.Minute,
		RecoveryHysteresis: 90 * time.Second,
		PreemptLead:        3 * time.Minute,
		Strategy:           strategy.StrategyConsumeFirst,
		ProbeUnknown:       false,
		Hover:              true,
		Manual:             true,
		CreditThreshold:    70,
		MaxAutoSpend:       25,
		WindowThreshold:    map[usage.WindowName]float64{usage.WindowFiveHour: 85},

		MCPSwitchWithoutElicitation: true,
		UpdateCheck:                 false,
		TUITheme:                    "dark",
		TUIGlyphs:                   "ascii",
		Codex: CodexConfig{
			Threshold:          65,
			Binary:             "/opt/codex/bin/codex",
			ProxyPort:          24680,
			CrossAccountReplay: true,
		},
	}
	// Equal rather than ==: Config carries the per-window table now, and a
	// struct holding a map is not comparable at all.
	if !cfg.Equal(want) {
		t.Errorf("Config() = %+v, want %+v", cfg, want)
	}
}

// `ccdad config list` prints the EFFECTIVE value of every key, set or not, so
// the formatter has to answer for a config rather than for a document.
func TestTheEffectiveValueOfEveryKeyIsFormattable(t *testing.T) {
	cfg := Defaults()
	want := map[string]string{
		keyThreshold:          "80",
		keyHysteresisPct:      "10",
		keyHeadroomRatio:      "2",
		keyCooldown:           "5m0s",
		keyRecoveryHysteresis: "5m0s",
		keyPreemptLead:        "6m0s",
		keyStrategy:           "headroom",
		keyProbeUnknown:       "true",
		keyHover:              "false",
		keyManual:             "false",
		keyCreditThreshold:    "80",
		keyMaxAutoSpend:       "0",

		keyMCPSwitchWithoutElicitation: "false",
		keyUpdateCheck:                 "true",

		keyCodexThreshold:          "80",
		keyCodexBinary:             "",
		keyCodexProxyPort:          "0",
		keyCodexCrossAccountReplay: "false",

		keyTUITheme:  "auto",
		keyTUIGlyphs: "auto",
	}
	for _, k := range Keys() {
		if _, named := want[k]; !named {
			t.Fatalf("Keys() names %q and this test gives it no expected value; a key added to the list is added here too", k)
		}
		got, err := cfg.Value(k)
		if err != nil {
			t.Fatalf("Value(%s) = %v", k, err)
		}
		if got != want[k] {
			t.Errorf("Value(%s) = %q, want %q", k, got, want[k])
		}
	}
	// A window with no key of its own is ranked against the top-level
	// threshold, so that is what its effective value has to be. Reporting
	// anything else would name a number the engine never uses.
	if got, err := cfg.Value("window_threshold.seven_day"); err != nil || got != "80" {
		t.Errorf("Value(window_threshold.seven_day) = %q, %v; want the 80 it falls back to", got, err)
	}
	if _, err := cfg.Value("threshhold"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Value(typo) = %v, want ErrUnknownKey", err)
	}
	if _, err := cfg.Value("window_threshold.typo"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Value(window_threshold.typo) = %v, want ErrUnknownKey", err)
	}
	// A window name is a key only inside its own table. Bare, it is a
	// top-level key this release does not have, and answering for it would
	// give one window two names — one of which nothing can set or unset.
	if _, err := cfg.Value("five_hour"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Value(five_hour) = %v, want ErrUnknownKey", err)
	}
}

// A section that still holds something — including a key this release does not
// know — stays, or the round-trip rule would have a hole in it.
func TestASectionThatStillHoldsSomethingSurvivesAnUnset(t *testing.T) {
	d, err := ParseDocument([]byte("[credit]\nmax_auto_spend = 25\nfuture_credit = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Unset(keyMaxAutoSpend); err != nil {
		t.Fatal(err)
	}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "future_credit") {
		t.Errorf("the unset took an unrelated key with it:\n%s", encoded)
	}
}

func TestADocumentReportsItsOwnUnknownKeys(t *testing.T) {
	d, err := ParseDocument([]byte("threshold = 70\nfuture_knob = 3\n[credit]\nmax_auto_spend = 1\nfuture_credit = 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := d.UnknownKeys()
	if len(got) != 2 || got[0] != "credit.future_credit" || got[1] != "future_knob" {
		t.Errorf("UnknownKeys() = %v, want [credit.future_credit future_knob]", got)
	}
}

// The file is hand-editable, so Value has to render what a person may have
// written and not only what Set stores.
func TestValueRendersHandWrittenLiterals(t *testing.T) {
	d, err := ParseDocument([]byte("threshold = 90\nstrategy = true\n\n[window_threshold]\nfive_hour = 85\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, set, err := d.Value(keyThreshold); err != nil || !set || got != "90" {
		t.Errorf("Value(threshold) = %q, %v, %v; want 90 for an integer literal", got, set, err)
	}
	// A window threshold is written by hand more often than by `config set` —
	// it lives in a table, and a person editing one types 85, not 85.0.
	if got, set, err := d.Value("window_threshold.five_hour"); err != nil || !set || got != "85" {
		t.Errorf("Value(window_threshold.five_hour) = %q, %v, %v; want 85", got, set, err)
	}
	// Nonsense for this key, and the loader refuses it — but `ccdad config get`
	// still has to be able to show the user what is in their file.
	if got, set, err := d.Value(keyStrategy); err != nil || !set || got != "true" {
		t.Errorf("Value(strategy) = %q, %v, %v; want the literal that is in the file", got, set, err)
	}
}

// The lock is what makes two `ccdad config set` calls safe. Without it both
// read, both change one key, and the second write drops the first one's key.
func TestASecondWriterWaitsForTheLockAndThenGivesUp(t *testing.T) {
	home := write(t, "")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := cclock.Acquire(filepath.Join(home, configLockDir), cclock.Options{
		Stale: configLockStale, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })

	saved := LockTimeout
	LockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { LockTimeout = saved })

	err = WithDocument(func(*Document) error {
		t.Error("the callback ran while another holder had the lock")
		return nil
	})
	if err == nil {
		t.Fatal("WithDocument() = nil while another process held the lock")
	}
	if !strings.Contains(err.Error(), FileName) {
		t.Errorf("error = %q, want it to name what could not be locked", err)
	}
}

// cclock catches a takeover two ways, and only one of them can see a steal that
// happened after the touch goroutine's last tick: the synchronous re-stat
// inside Release. Throwing Release's answer away reports success for the one
// write that actually raced.
func TestWithDocumentReportsALockStolenDuringTheHold(t *testing.T) {
	home := write(t, "")
	lockDir := filepath.Join(home, configLockDir)

	err := WithDocument(func(d *Document) error {
		if err := d.Set(keyThreshold, "55"); err != nil {
			return err
		}
		// Another process deems the lock stale and takes it over: rmdir then
		// mkdir, which is what cclock.Acquire's steal path does.
		if err := os.Remove(lockDir); err != nil {
			return err
		}
		if err := os.Mkdir(lockDir, 0o700); err != nil {
			return err
		}
		past := time.Now().Add(-time.Second)
		return os.Chtimes(lockDir, past, past)
	})

	if err == nil {
		t.Fatal("WithDocument() reported success for a write that raced a lock takeover")
	}
	if !errors.Is(err, cclock.ErrCompromised) {
		t.Errorf("error = %v, want it to carry cclock.ErrCompromised", err)
	}
	_ = os.Remove(lockDir)
}

// A callback that fails still reports its own error, and joining Release's
// answer must not swallow it.
func TestWithDocumentKeepsTheCallbacksError(t *testing.T) {
	write(t, "")
	boom := errors.New("boom")

	if err := WithDocument(func(*Document) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("error = %v, want boom", err)
	}
}

func TestAWindowThresholdIsSetAndReadBackByItsDottedName(t *testing.T) {
	d := newDocument()
	if err := d.Set("window_threshold.five_hour", "85"); err != nil {
		t.Fatalf("Set(window_threshold.five_hour) = %v", err)
	}
	got, set, err := d.Value("window_threshold.five_hour")
	if err != nil || !set || got != "85" {
		t.Fatalf("Value() = %q, set %v, err %v; want 85", got, set, err)
	}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "[window_threshold]") {
		t.Errorf("encoded document has no [window_threshold] table:\n%s", encoded)
	}
	cfg, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WindowThreshold[usage.WindowFiveHour] != 85 {
		t.Errorf("WindowThreshold = %v, want five_hour at 85", cfg.WindowThreshold)
	}
}

func TestAWindowNameThatIsNotAWindowIsAnUnknownKey(t *testing.T) {
	for _, key := range []string{
		"window_threshold.typo",
		// A window the endpoint really reports, and refused all the same: its
		// resets_at is an expiry rather than a rollover, so RateLimitWindows
		// leaves it out of the ranking and a threshold on it would govern
		// nothing.
		"window_threshold.cinder_cove",
		// The scoped prefixes with no display name after them. A scoped window
		// with no name is dropped by ScopedWindows, so a threshold under one
		// could never bind.
		"window_threshold.weekly_scoped:model:",
		"window_threshold.weekly_scoped:",
		"window_threshold.",
		// The table itself is not a value.
		"window_threshold",
	} {
		d := newDocument()
		if err := d.Set(key, "85"); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("Set(%s) = %v, want ErrUnknownKey", key, err)
		}
		if _, _, err := d.Value(key); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("Value(%s) = %v, want ErrUnknownKey", key, err)
		}
		if _, err := d.Unset(key); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("Unset(%s) = %v, want ErrUnknownKey", key, err)
		}
	}
}

// `ccdad config set` reads the key back to report what the file now holds. A
// Value that refused what Set accepted would leave that echo showing the string
// the user typed instead of the value that was stored, so the two answer from
// one predicate and this is what keeps them there.
func TestSetAndValueAgreeOnEveryWindowName(t *testing.T) {
	for _, name := range []string{
		"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet",
		"weekly_scoped:model:Opus 4.5", "weekly_scoped:surface:Claude Code",
		"cinder_cove", "typo", "weekly_scoped:model:", "",
	} {
		key := "window_threshold." + name
		d := newDocument()
		setErr := d.Set(key, "85")
		_, _, valueErr := d.Value(key)
		if errors.Is(setErr, ErrUnknownKey) != errors.Is(valueErr, ErrUnknownKey) {
			t.Errorf("%q: Set = %v, Value = %v; the two guards disagree", key, setErr, valueErr)
		}
	}
}

func TestTheEffectiveWindowThresholdFallsBackToTheTopLevelOne(t *testing.T) {
	cfg, err := Parse([]byte("threshold = 70\n\n[window_threshold]\nfive_hour = 85\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.Value("window_threshold.five_hour"); err != nil || got != "85" {
		t.Errorf("Value(five_hour) = %q, %v; want the file's 85", got, err)
	}
	if got, err := cfg.Value("window_threshold.seven_day"); err != nil || got != "70" {
		t.Errorf("Value(seven_day) = %q, %v; want the 70 it falls back to", got, err)
	}
	if _, err := cfg.Value("window_threshold.typo"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Value(typo) = %v, want ErrUnknownKey", err)
	}
}

// A scoped window is named after a model or a surface the SERVER invented, so
// its key carries spaces, dots and possibly quotes. It has to survive the
// encoder, and `Opus 4.5` also proves the dotted key is split on its FIRST dot.
func TestAScopedWindowNameSurvivesTheRoundTrip(t *testing.T) {
	const key = "window_threshold.weekly_scoped:model:Opus 4.5"
	d := newDocument()
	if err := d.Set(key, "40"); err != nil {
		t.Fatal(err)
	}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseDocument(encoded)
	if err != nil {
		t.Fatalf("re-parsing a document carrying a scoped window name: %v\n%s", err, encoded)
	}
	got, set, err := back.Value(key)
	if err != nil || !set || got != "40" {
		t.Errorf("Value() = %q, set %v, err %v; want 40 back out of\n%s", got, set, err, encoded)
	}
}

// go-toml marshals an empty map as a bare header, so a section emptied by an
// unset has to go with it or the next `ccdad config list` reads a table that is
// not there.
func TestUnsettingTheLastWindowLeavesNoEmptyHeaderBehind(t *testing.T) {
	d, err := ParseDocument([]byte("threshold = 80\n\n[window_threshold]\nfive_hour = 85\n"))
	if err != nil {
		t.Fatal(err)
	}
	removed, err := d.Unset("window_threshold.five_hour")
	if err != nil || !removed {
		t.Fatalf("Unset() = %v, %v; want true", removed, err)
	}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "[window_threshold]") {
		t.Errorf("an empty [window_threshold] header was left behind:\n%s", encoded)
	}
}

func TestDocumentKeysAddsOnlyTheWindowsTheDocumentNames(t *testing.T) {
	d, err := ParseDocument([]byte("threshold = 80\n\n[window_threshold]\nseven_day = 60\nfive_hour = 85\nthirty_day = 50\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := append(Keys(), "window_threshold.five_hour", "window_threshold.seven_day")
	if got := d.Keys(); !slices.Equal(got, want) {
		t.Errorf("Keys() = %v, want %v: the fixed keys, then the windows the file names, sorted", got, want)
	}

	// No table at all, which is every machine that never wrote one: the
	// listing is exactly the fixed keys.
	if got := newDocument().Keys(); !slices.Equal(got, Keys()) {
		t.Errorf("Keys() on an empty document = %v, want %v", got, Keys())
	}

	// A header with nothing under it is the same answer. The rows come from
	// the keys in the table, not from the table existing.
	bare, err := ParseDocument([]byte("[window_threshold]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := bare.Keys(); !slices.Equal(got, Keys()) {
		t.Errorf("Keys() with an empty table = %v, want %v", got, Keys())
	}
	if got := bare.UnknownKeys(); len(got) != 0 {
		t.Errorf("UnknownKeys() = %v; an empty table of a section this release knows is not an unknown key", got)
	}
}

// Every key that is a utilization percent is held to the top-level threshold's
// bounds. It is written as a comparison against `threshold` itself rather than
// as a list of bad numbers, because the property is that they cannot drift: a
// key given a number the loader would refuse is a value `ccdad config set`
// accepted and the engine then ignores the whole file for.
func TestEveryPercentKeyIsBoundedExactlyAsTheTopLevelThresholdIs(t *testing.T) {
	for _, key := range []string{"window_threshold.five_hour", keyCreditThreshold} {
		for _, value := range []string{"85", "0.5", "100", "0", "-5", "101", "inf", "nan", "ninety"} {
			top := newDocument().Set(keyThreshold, value)
			got := newDocument().Set(key, value)
			if (top == nil) != (got == nil) {
				t.Errorf("%q: Set(threshold) = %v, Set(%s) = %v; the two bounds have drifted",
					value, top, key, got)
			}
		}
	}
}

// Set validates the way the LOADER would, and preempt_lead is the key where
// that is not its neighbours' rule: zero switches the pre-emptive switch off,
// which is a value a user may choose, while a zero cooldown is an anti-flap
// mechanism disabled. A set LOOSER than the loader writes a file the engine
// then ignores wholesale; a set STRICTER than it refuses a value the file
// accepts, which leaves a hand edit as the only way to write it. Both are
// failures of the same agreement, so the test asserts the agreement itself.
func TestSetAndTheLoaderAgreeOnEveryDurationKey(t *testing.T) {
	for _, key := range []string{keyCooldown, keyRecoveryHysteresis, keyPreemptLead} {
		for _, value := range []string{"90s", "2m", "0", "0s", "-2m", "300", "soon"} {
			setErr := newDocument().Set(key, value)
			_, parseErr := Parse([]byte(key + ` = "` + value + `"`))
			if (setErr == nil) != (parseErr == nil) {
				t.Errorf("%s = %q: Set = %v, Parse = %v", key, value, setErr, parseErr)
			}
		}
	}
}
