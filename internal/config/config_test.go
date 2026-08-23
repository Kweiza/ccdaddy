package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// write puts a config.toml in an isolated CCDAD_HOME and returns the home.
func write(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CCDAD_HOME", home)
	if body != "" {
		if err := os.WriteFile(filepath.Join(home, FileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// The five cases the item names as its Done criteria: missing file, partial
// config, inf, nan, syntax error.

func TestAMissingFileIsTheFullDefaultSet(t *testing.T) {
	write(t, "")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() on a machine with no config file failed: %v", err)
	}
	if got != Defaults() {
		t.Errorf("Load() = %+v, want the defaults %+v", got, Defaults())
	}
}

func TestAPartialConfigLeavesEveryOtherKeyAtItsDefault(t *testing.T) {
	write(t, "threshold = 65.0\n")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	want := Defaults()
	want.Threshold = 65
	if got != want {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

func TestAnInfiniteCeilingIsRefusedAtParseTime(t *testing.T) {
	_, err := Parse([]byte("[credit]\nmax_auto_spend = inf\n"))
	if err == nil {
		t.Fatal("max_auto_spend = inf parsed without error; the IsInf check must fire at read time")
	}
	if !strings.Contains(err.Error(), "max_auto_spend") {
		t.Errorf("error = %q, want it to name max_auto_spend", err)
	}
}

func TestANaNCeilingIsRefusedAtParseTime(t *testing.T) {
	_, err := Parse([]byte("[credit]\nmax_auto_spend = nan\n"))
	if err == nil {
		t.Fatal("max_auto_spend = nan parsed without error; every NaN comparison is false, so it slips a plain <= 0")
	}
}

func TestASyntaxErrorIsReportedRatherThanSilentlyIgnored(t *testing.T) {
	_, err := Parse([]byte("threshold = = 80\n"))
	if err == nil {
		t.Fatal("a syntactically broken file parsed without error")
	}
}

// The second trap the item names: absent and explicitly-zero must not be the
// same value, and they are safe in opposite directions.

func TestAnExplicitZeroCeilingIsHonouredAsTheRefusingDefault(t *testing.T) {
	got, err := Parse([]byte("[credit]\nmax_auto_spend = 0.0\n"))
	if err != nil {
		t.Fatalf("Parse() = %v; 0 is max_auto_spend's own default and must be a legal value", err)
	}
	if got.MaxAutoSpend != 0 {
		t.Errorf("MaxAutoSpend = %v, want 0", got.MaxAutoSpend)
	}
}

func TestAnExplicitZeroHysteresisIsRefusedRatherThanSilentlyDisablingAntiFlap(t *testing.T) {
	_, err := Parse([]byte("hysteresis_pct = 0.0\n"))
	if err == nil {
		t.Fatal("hysteresis_pct = 0 was accepted; an anti-flap margin cannot be switched off, " +
			"and strategy.Config.withDefaults would silently replace it anyway")
	}
}

func TestAnExplicitZeroCooldownIsRefused(t *testing.T) {
	if _, err := Parse([]byte(`cooldown = "0s"`)); err == nil {
		t.Fatal("cooldown = 0s was accepted; a zero cooldown is a switch storm")
	}
}

// The message is the user's only guide back to a working file, so it has to say
// what the value should look like rather than only that it was refused.
func TestANonDurationStringSaysWhatADurationLooksLike(t *testing.T) {
	_, err := Parse([]byte(`cooldown = "soon"`))
	if err == nil {
		t.Fatal(`cooldown = "soon" was accepted`)
	}
	if !strings.Contains(err.Error(), "5m") {
		t.Errorf("error = %q, want an example of the form it takes", err)
	}
}

func TestANegativeCooldownIsRefused(t *testing.T) {
	if _, err := Parse([]byte(`cooldown = "-5m"`)); err == nil {
		t.Fatal("a negative cooldown was accepted")
	}
}

func TestEveryFloatKeyRefusesANonFiniteValue(t *testing.T) {
	for _, key := range []string{"threshold", "hysteresis_pct", "headroom_ratio"} {
		for _, value := range []string{"inf", "-inf", "nan"} {
			if _, err := Parse([]byte(key + " = " + value + "\n")); err == nil {
				t.Errorf("%s = %s was accepted", key, value)
			}
		}
	}
}

func TestAThresholdOutsideTheUtilizationRangeIsRefused(t *testing.T) {
	for _, body := range []string{"threshold = 0.0", "threshold = -1.0", "threshold = 101.0"} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%q was accepted; a threshold is a utilization percent", body)
		}
	}
}

func TestAHeadroomRatioBelowOneIsRefused(t *testing.T) {
	// Below 1.0 the "margin" lets a candidate with LESS headroom than the live
	// account clear it, which inverts the mechanism rather than loosening it.
	if _, err := Parse([]byte("headroom_ratio = 0.5")); err == nil {
		t.Fatal("headroom_ratio = 0.5 was accepted")
	}
	if _, err := Parse([]byte("headroom_ratio = 1.0")); err != nil {
		t.Fatalf("headroom_ratio = 1.0 was refused (%v); 1.0 is 'no multiplicative margin', which is coherent", err)
	}
}

func TestANegativeCeilingIsRefused(t *testing.T) {
	if _, err := Parse([]byte("[credit]\nmax_auto_spend = -1.0")); err == nil {
		t.Fatal("a negative max_auto_spend was accepted")
	}
}

func TestAKnownStrategyNameIsParsedAndAnUnknownOneIsRefused(t *testing.T) {
	got, err := Parse([]byte(`strategy = "consume-first"`))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if got.Strategy != strategy.StrategyConsumeFirst {
		t.Errorf("Strategy = %v, want consume-first", got.Strategy)
	}
	_, err = Parse([]byte(`strategy = "consume-frist"`))
	if err == nil {
		t.Fatal("a misspelled strategy was accepted; a typo that silently runs the wrong strategy is the cswap behaviour the exit contract exists to fix")
	}
	if !strings.Contains(err.Error(), "consume-first") {
		t.Errorf("error = %q, want it to list the strategies that do exist", err)
	}
}

func TestAValueOfTheWrongTypeIsRefused(t *testing.T) {
	if _, err := Parse([]byte(`threshold = "80"`)); err == nil {
		t.Fatal("a quoted threshold was accepted; go-toml would not decode a string into a float64")
	}
	if _, err := Parse([]byte(`cooldown = 300`)); err == nil {
		t.Fatal("a bare-integer cooldown was accepted; durations are Go duration strings, and 300 is ambiguous between seconds and nanoseconds")
	}
}

// The defaults must be the SAME numbers the engine falls back to on its own, or
// a config file that omits a key would change behaviour by omitting it.
func TestTheDefaultsAreTheEnginesOwnDefaults(t *testing.T) {
	d := Defaults()
	engine := d.StrategyConfig()
	zero := strategy.Config{
		HysteresisPct:      strategy.DefaultHysteresisPct,
		HeadroomRatio:      strategy.DefaultHeadroomRatio,
		Cooldown:           strategy.DefaultCooldown,
		RecoveryHysteresis: strategy.DefaultRecoveryHysteresis,
		MaxAutoSpend:       0,
	}
	if engine != zero {
		t.Errorf("Defaults().StrategyConfig() = %+v, want the engine's own defaults %+v", engine, zero)
	}
	if d.Threshold != strategy.DefaultThreshold {
		t.Errorf("Threshold = %v, want strategy.DefaultThreshold %v", d.Threshold, strategy.DefaultThreshold)
	}
	if d.Strategy != strategy.StrategyHeadroom {
		t.Errorf("Strategy = %v, want headroom", d.Strategy)
	}
	if d.MaxAutoSpend != 0 {
		t.Errorf("MaxAutoSpend = %v, want 0 — the explicit opt-in the credit gate requires", d.MaxAutoSpend)
	}
}

func TestTheConfigCarriesIntoARankingPass(t *testing.T) {
	cfg, err := Parse([]byte("threshold = 55.0\nstrategy = \"consume-first\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	o := cfg.RankOptions(now)
	if o.Threshold != 55 || o.Strategy != strategy.StrategyConsumeFirst || !o.Now.Equal(now) {
		t.Errorf("RankOptions() = %+v, want the file's threshold and strategy at now", o)
	}
}

func TestEveryKnobReachesTheEngineConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
hysteresis_pct = 12.5
headroom_ratio = 3.0
cooldown = "7m"
recovery_hysteresis = "90s"
[credit]
max_auto_spend = 25.0
`))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.StrategyConfig()
	want := strategy.Config{
		HysteresisPct:      12.5,
		HeadroomRatio:      3,
		Cooldown:           7 * time.Minute,
		RecoveryHysteresis: 90 * time.Second,
		MaxAutoSpend:       25,
	}
	if got != want {
		t.Errorf("StrategyConfig() = %+v, want %+v", got, want)
	}
}

// Unknown keys are the compatibility rule: a file written by a newer ccdad must
// not stop an older one, so they are ignored by the loader and reported
// separately rather than being an error.
func TestAnUnknownKeyIsIgnoredByTheLoaderAndReportedSeparately(t *testing.T) {
	raw := []byte("threshold = 70.0\nfuture_knob = 3\n")
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse() refused an unknown key (%v); an older ccdad must keep running against a newer file", err)
	}
	if cfg.Threshold != 70 {
		t.Errorf("Threshold = %v, want 70", cfg.Threshold)
	}
	unknown, err := UnknownKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 1 || unknown[0] != "future_knob" {
		t.Errorf("UnknownKeys() = %v, want [future_knob]", unknown)
	}
}

// A key inside a table this release DOES know is reported by its dotted name.
// A whole table it does not know is reported once, by the table's name, since
// naming every key under it says nothing more.
func TestUnknownKeysReportsADocumentItCannotRead(t *testing.T) {
	// Answering "no unknown keys" for a file nothing could decode would report
	// a clean bill of health for a document that has not been read at all.
	if _, err := UnknownKeys([]byte("threshold = = 9")); err == nil {
		t.Fatal("UnknownKeys() = nil error for an unparseable document")
	}
}

func TestUnknownKeysReachesInsideAKnownSection(t *testing.T) {
	unknown, err := UnknownKeys([]byte("[credit]\nmax_auto_spend = 1\nfuture_credit = 2\n\n[future]\na = 1\nb = 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 2 || unknown[0] != "credit.future_credit" || unknown[1] != "future" {
		t.Errorf("UnknownKeys() = %v, want [credit.future_credit future]", unknown)
	}
}

func TestAnUnreadableConfigIsAnErrorRatherThanSilentDefaults(t *testing.T) {
	home := write(t, "threshold = 80.0\n")
	if err := os.Chmod(filepath.Join(home, FileName), 0o000); err != nil {
		t.Skipf("cannot make the file unreadable here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, FileName), 0o600) })
	// Probe rather than trust the chmod. os.Chmod REPORTS success on Windows
	// and changes nothing but the read-only attribute, and root reads a
	// mode-000 file anywhere -- either way the file stays readable and this
	// test has nothing left to assert. os.Geteuid() cannot see the first
	// case: it answers -1 on Windows.
	if _, err := os.ReadFile(filepath.Join(home, FileName)); err == nil {
		t.Skip("the file is still readable here, so there is nothing to refuse")
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load() reported the defaults for a file it could not read; that hides a config the user believes is in force")
	}
}

func TestARelativeStoreRootIsRefused(t *testing.T) {
	t.Setenv("CCDAD_HOME", "relative-ccdad")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted a relative store root; the config would then come from whatever directory ccdad was run in")
	}
}

// The re-read the tick loop performs: the daemon picks up an external edit, and
// a broken one leaves it running on the last good config.

func TestAReloaderPicksUpAnExternalEdit(t *testing.T) {
	home := write(t, "threshold = 70.0\n")
	r := NewReloader()

	first, err := r.Reload()
	if err != nil || first.Threshold != 70 {
		t.Fatalf("Reload() = %+v, %v; want threshold 70", first, err)
	}
	if err := os.WriteFile(filepath.Join(home, FileName), []byte("threshold = 45.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := r.Reload()
	if err != nil {
		t.Fatalf("Reload() = %v", err)
	}
	if second.Threshold != 45 {
		t.Errorf("Threshold = %v, want the edited 45", second.Threshold)
	}
}

func TestABrokenEditLeavesTheReloaderOnTheLastGoodConfig(t *testing.T) {
	home := write(t, "threshold = 70.0\n")
	r := NewReloader()
	if _, err := r.Reload(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(home, FileName), []byte("threshold = = 45\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := r.Reload()
	if err == nil {
		t.Fatal("Reload() hid a broken edit; the daemon has to be able to warn about it")
	}
	if got.Threshold != 70 {
		t.Errorf("Threshold = %v, want the last good 70 — a broken hand-edit must not stop the daemon switching", got.Threshold)
	}
}

func TestADeletedFileReturnsTheReloaderToTheDefaults(t *testing.T) {
	home := write(t, "threshold = 70.0\n")
	r := NewReloader()
	if _, err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, FileName)); err != nil {
		t.Fatal(err)
	}
	got, err := r.Reload()
	if err != nil {
		t.Fatalf("Reload() = %v; deleting the file is a legal edit", err)
	}
	if got != Defaults() {
		t.Errorf("Reload() = %+v, want the defaults", got)
	}
}

// A change is detected on the file's BYTES. Two `ccdad config set` calls inside
// one filesystem timestamp tick are ordinary, and an mtime- or size-based probe
// would leave the daemon running on the old value until something else touched
// the file.
func TestASameSizeEditInsideOneTimestampTickIsPickedUp(t *testing.T) {
	home := write(t, "threshold = 70.0\n")
	r := NewReloader()
	if _, err := r.Reload(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(home, FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same byte count, and the mtime is put back to what it was.
	if err := os.WriteFile(path, []byte("threshold = 45.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	got, err := r.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if got.Threshold != 45 {
		t.Errorf("Threshold = %v, want the edited 45", got.Threshold)
	}
}

// The key namespace is a compatibility commitment and the closed set that keeps
// a secret from ever being settable. Both make this list the deliberate edit.
func TestTheKeySetIsClosed(t *testing.T) {
	want := []string{
		"threshold", "hysteresis_pct", "headroom_ratio",
		"cooldown", "recovery_hysteresis", "strategy", "credit.max_auto_spend",
	}
	got := Keys()
	if len(got) != len(want) {
		t.Fatalf("Keys() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Keys() = %v, want %v", got, want)
		}
	}
}

func TestPathIsInsideTheStoreHome(t *testing.T) {
	home := write(t, "")
	if got, want := mustPath(Path()), filepath.Join(home, FileName); got != want {
		t.Errorf("mustPath(Path()) = %q, want %q", got, want)
	}
}

// A NaN that reached Config would rank as "has room" and lose no comparison, so
// the guard is asserted on the value the engine actually receives too.
func TestNoNonFiniteValueCanReachTheEngine(t *testing.T) {
	for _, body := range []string{
		"threshold = nan", "hysteresis_pct = inf", "headroom_ratio = nan",
		"[credit]\nmax_auto_spend = inf",
	} {
		cfg, err := Parse([]byte(body))
		if err == nil {
			t.Fatalf("%q parsed", body)
		}
		if cfg != (Config{}) {
			t.Errorf("Parse(%q) returned %+v alongside its error; a refused file must yield nothing usable", body, cfg)
		}
	}
}

// A read failure is transient — a file locked by a backup, a directory briefly
// unreadable — and it says nothing about the CONTENT. Caching it alongside the
// bytes would leave the daemon reporting a problem that has gone away for as
// long as nobody edits the file again.
func TestATransientReadFailureIsNotRememberedOnceItClears(t *testing.T) {
	home := write(t, "threshold = 70.0\n")
	path := filepath.Join(home, FileName)
	r := NewReloader()
	if _, err := r.Reload(); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot make the file unreadable here: %v", err)
	}
	// Probe rather than trust the chmod. os.Chmod REPORTS success on Windows
	// and changes nothing but the read-only attribute, and root reads a
	// mode-000 file anywhere -- either way the file stays readable and this
	// test has nothing left to assert. os.Geteuid() cannot see the first
	// case: it answers -1 on Windows.
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("the file is still readable here, so there is nothing to refuse")
	}
	if _, err := r.Reload(); err == nil {
		t.Fatal("Reload() hid an unreadable file")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := r.Reload()
	if err != nil {
		t.Errorf("Reload() = %v after the file became readable again with the same contents", err)
	}
	if got.Threshold != 70 {
		t.Errorf("Threshold = %v, want 70", got.Threshold)
	}
}
