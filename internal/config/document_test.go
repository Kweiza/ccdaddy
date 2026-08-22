package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclock"
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
	if cfg != Defaults() {
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

// §4.2 rule 3, applied to this file: decoding into a typed struct and
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
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600", perm)
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
	values := map[string]string{
		keyThreshold:          "55",
		keyHysteresisPct:      "12.5",
		keyHeadroomRatio:      "3",
		keyCooldown:           "7m0s",
		keyRecoveryHysteresis: "1m30s",
		keyStrategy:           "consume-first",
		keyMaxAutoSpend:       "25",
	}
	d := newDocument()
	for _, k := range Keys() {
		if err := d.Set(k, values[k]); err != nil {
			t.Fatalf("Set(%s) = %v", k, err)
		}
	}
	for _, k := range Keys() {
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
	want := Config{
		Threshold: 55, HysteresisPct: 12.5, HeadroomRatio: 3,
		Cooldown: 7 * time.Minute, RecoveryHysteresis: 90 * time.Second,
		Strategy: 1, MaxAutoSpend: 25,
	}
	if cfg != want {
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
		keyStrategy:           "headroom",
		keyMaxAutoSpend:       "0",
	}
	for _, k := range Keys() {
		got, err := cfg.Value(k)
		if err != nil {
			t.Fatalf("Value(%s) = %v", k, err)
		}
		if got != want[k] {
			t.Errorf("Value(%s) = %q, want %q", k, got, want[k])
		}
	}
	if _, err := cfg.Value("threshhold"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("Value(typo) = %v, want ErrUnknownKey", err)
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
	d, err := ParseDocument([]byte("threshold = 90\nstrategy = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, set, err := d.Value(keyThreshold); err != nil || !set || got != "90" {
		t.Errorf("Value(threshold) = %q, %v, %v; want 90 for an integer literal", got, set, err)
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
