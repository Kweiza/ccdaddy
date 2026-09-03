package config

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
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
	if !got.Equal(Defaults()) {
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
	if !got.Equal(want) {
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
	for _, body := range []string{`hover = "true"`, `probe_unknown = "false"`} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s was accepted; go-toml will not decode a string into a bool, and a quoted bool is exactly how a user writes one by mistake", body)
		}
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
	if d.CreditThreshold != strategy.DefaultCreditThreshold {
		t.Errorf("CreditThreshold = %v, want strategy.DefaultCreditThreshold %v", d.CreditThreshold, strategy.DefaultCreditThreshold)
	}
	if !d.ProbeUnknown {
		t.Error("ProbeUnknown = false; an account whose window has never been used has no reading at all, and the probe is the only way to get one")
	}
	if d.Hover {
		t.Error("Hover = true; a mode that overrides every tuning key has to be asked for")
	}
	if d.WindowThreshold != nil {
		t.Errorf("WindowThreshold = %v, want nil — nil is 'every window uses threshold' and needs no allocation to say so", d.WindowThreshold)
	}
}

// preempt_lead has no engine constant behind it, and that is the decision
// rather than an omission: strategy reads a zero PreemptLead as the pre-emptive
// switch off, so a default living there would turn the mechanism back on for a
// caller that meant to leave it off. The number is config's own, and this is
// the pin on it.
// The lead is derived from the cadence an AT-RISK account actually polls at, and
// that is the danger band's, not the urgent path's. It used to be two minutes on
// the reading that 60 s was the fastest an account near its ceiling would see;
// when the band moved to pollpolicy.DangerInterval a two-minute lead became
// shorter than one poll interval, which is the case defaults.go has always said
// the mechanism exists to prevent — the projection overtaken between two readings
// and the switch landing after the session was already cut off.
func TestThePreemptLeadDefaultIsConfigsOwnNumber(t *testing.T) {
	got := Defaults().PreemptLead
	if want := 6 * time.Minute; got != want {
		t.Errorf("Defaults().PreemptLead = %v, want %v — two of the cadence an at-risk "+
			"account polls at, which is what is left for the switch to be decided and picked "+
			"up after the horizon has counted the wait for the next reading", got, want)
	}
	if got < pollpolicy.DangerInterval {
		t.Errorf("Defaults().PreemptLead = %v is shorter than the %v cadence an account "+
			"near its ceiling polls at, so the projection is overtaken between two readings",
			got, pollpolicy.DangerInterval)
	}
}

func TestTheConfigCarriesIntoARankingPass(t *testing.T) {
	cfg, err := Parse([]byte("threshold = 55.0\nstrategy = \"consume-first\"\npreempt_lead = \"90s\"\n[window_threshold]\nseven_day = 40.0\n[credit]\nthreshold = 65.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	o := cfg.RankOptions(now)
	if o.Threshold != 55 || o.Strategy != strategy.StrategyConsumeFirst || !o.Now.Equal(now) {
		t.Errorf("RankOptions() = %+v, want the file's threshold and strategy at now", o)
	}
	if o.CreditThreshold != 65 {
		t.Errorf("RankOptions().CreditThreshold = %v, want 65", o.CreditThreshold)
	}
	if o.PreemptLead != 90*time.Second {
		t.Errorf("RankOptions().PreemptLead = %v, want 90s; a lead the ranking never receives is pre-emption switched off by accident", o.PreemptLead)
	}
	if got := o.WindowThreshold[usage.WindowSevenDay]; got != 40 {
		t.Errorf("RankOptions().WindowThreshold[seven_day] = %v, want 40; a per-window threshold the ranking never receives is a key that does nothing", got)
	}
}

func TestEveryKnobReachesTheEngineConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
hysteresis_pct = 12.5
headroom_ratio = 3.0
cooldown = "7m"
recovery_hysteresis = "90s"
manual = true
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
		// A MODE rather than a knob, and here for exactly that reason: it is
		// the one field in this struct whose whole job is the wire from
		// config.toml to Decide, and nothing else in the tree asserts that
		// wire. Dropped from StrategyConfig, `ccdad manual on` writes the key,
		// every table says the mode is on, and the engine keeps switching.
		Manual: true,
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

	// window_threshold is the second section this release nests keys under,
	// and the same rule reaches inside it: a name that is not a window is
	// reported by its dotted name, while the windows themselves are not
	// reported at all. Before it was a known section the whole table was
	// reported as one unrecognized key, so `ccdad doctor` told the user a
	// working config was being ignored.
	unknown, err = UnknownKeys([]byte("[window_threshold]\nfive_hour = 85\n\"weekly_scoped:model:Opus 4.5\" = 40\ncinder_cove = 50\nthirty_day = 60\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 2 || unknown[0] != "window_threshold.cinder_cove" || unknown[1] != "window_threshold.thirty_day" {
		t.Errorf("UnknownKeys() = %v, want [window_threshold.cinder_cove window_threshold.thirty_day]", unknown)
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
	if !got.Equal(Defaults()) {
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
		"cooldown", "recovery_hysteresis", "preempt_lead", "strategy",
		"probe_unknown", "hover", "manual", "mcp_switch_without_elicitation", "update_check",
		"credit.threshold", "credit.max_auto_spend",
		"codex.threshold", "codex.binary", "codex.proxy_port", "codex.cross_account_replay",
		"tui.theme", "tui.glyphs",
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

	// The three keys this package exports by name for another package to print.
	// An exported constant that drifted from the namespace would have the
	// printing package naming a key `ccdad config set` then refuses, and the
	// caller's own assertion could not see it: it compares the constant with
	// itself.
	for _, exported := range []string{KeyHover, KeyMCPSwitchWithoutElicitation, KeyUpdateCheck} {
		if !slices.Contains(Keys(), exported) {
			t.Errorf("the exported key %q is not in the settable namespace: %v", exported, Keys())
		}
	}

	// window_threshold is the one settable name the list above cannot carry: a
	// scoped window is named after a model or a surface the server invented, so
	// the legal names are not knowable in advance. It is closed one level down
	// instead, by what a window may be called. Without this half the list would
	// no longer describe the whole surface and the gate would be pinning half a
	// namespace.
	d := newDocument()
	if err := d.Set("window_threshold.five_hour", "85"); err != nil {
		t.Errorf("Set(window_threshold.five_hour) = %v, want it accepted", err)
	}
	// The third table, and the first one that governs nothing the engine does.
	// It is closed by NAME the way [credit] is -- every key under it is in the
	// list above -- which is why isKnownKey needs no `tui.` arm to match the
	// `window_threshold.` one.
	if err := d.Set("tui.theme", "dark"); err != nil {
		t.Errorf("Set(tui.theme) = %v, want it accepted", err)
	}
	for _, key := range []string{
		"window_threshold.typo", // not a window
		"window_threshold",      // the table itself is not a value
		"credit.future",         // the other section stays closed by name
		"tui.future",            // and so does the display one
		"future.a",              // and no fourth section is open
	} {
		if err := d.Set(key, "1"); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("Set(%s) = %v, want ErrUnknownKey", key, err)
		}
	}
}

// Every window the ranking can bind on has to be one a threshold can be
// attached to, or the config offers a knob for some windows and silently not
// for others.
//
// The refusal is rendered once and checked against each of them: a window that
// is settable and a refusal that does not name it are the same defect seen from
// two sides, because the refusal is the only place a user finds the key.
func TestEveryRankedWindowIsSettableAndCinderCoveIsNot(t *testing.T) {
	refusal := unknownKey(windowThresholdSection + ".typo").Error()
	for _, name := range usage.RateLimitWindowNames() {
		key := windowThresholdSection + "." + string(name)
		if !isKnownKey(key) {
			t.Errorf("%s is a window the ranking binds on and %s is refused", name, key)
		}
		if !strings.Contains(refusal, string(name)) {
			t.Errorf("the refusal message does not name %s, so a user cannot find the key from it", name)
		}
	}
	if isKnownKey(windowThresholdSection + "." + string(usage.WindowCinderCove)) {
		t.Errorf("%s is settable; its resets_at is an expiry, so nothing ranks it and a threshold on it would do nothing",
			usage.WindowCinderCove)
	}
	// cinder_cove's own refusal is a different sentence from a misspelling's,
	// and flattening the two would tell a user to check their spelling of a
	// name they spelled correctly.
	if got := unknownKey(windowThresholdSection + "." + string(usage.WindowCinderCove)).Error(); !strings.Contains(got, "expiry") {
		t.Errorf("refusing %s says %q; it has to say what the window IS rather than that it is misspelled",
			usage.WindowCinderCove, got)
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
		if !cfg.Equal(Config{}) {
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

func TestTheNewKeysAreParsedFromTheFile(t *testing.T) {
	cfg, err := Parse([]byte(`
preempt_lead = "90s"
probe_unknown = false
hover = true

[window_threshold]
five_hour = 85
seven_day = 60
"weekly_scoped:model:Opus 4.5" = 40

[credit]
threshold = 55.0
`))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if cfg.PreemptLead != 90*time.Second {
		t.Errorf("PreemptLead = %v, want 90s", cfg.PreemptLead)
	}
	if cfg.ProbeUnknown {
		t.Error("ProbeUnknown = true; the file said false, and the default is the opposite of the file")
	}
	if !cfg.Hover {
		t.Error("Hover = false; the file said true")
	}
	if cfg.CreditThreshold != 55 {
		t.Errorf("CreditThreshold = %v, want 55", cfg.CreditThreshold)
	}
	want := map[usage.WindowName]float64{
		usage.WindowFiveHour:           85,
		usage.WindowSevenDay:           60,
		"weekly_scoped:model:Opus 4.5": 40,
	}
	if !maps.Equal(cfg.WindowThreshold, want) {
		t.Errorf("WindowThreshold = %v, want %v", cfg.WindowThreshold, want)
	}
}

// The table is an OVERRIDE, not a replacement: a window with no key of its own
// keeps using `threshold`. Without the fallback, writing one key would silently
// set every other window to zero, which reads as "spent" for all of them.
func TestAWindowWithNoKeyOfItsOwnFallsBackToTheGlobalThreshold(t *testing.T) {
	cfg, err := Parse([]byte("threshold = 70.0\n[window_threshold]\nseven_day = 60.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	th := cfg.Thresholds()
	if got := th.For(usage.WindowSevenDay); got != 60 {
		t.Errorf("Thresholds().For(seven_day) = %v, want the key's own 60", got)
	}
	if got := th.For(usage.WindowFiveHour); got != 70 {
		t.Errorf("Thresholds().For(five_hour) = %v, want the file's threshold 70", got)
	}
	if got := th.CreditThreshold(); got != strategy.DefaultCreditThreshold {
		t.Errorf("Thresholds().CreditThreshold() = %v, want %v; the file said nothing about credits", got, strategy.DefaultCreditThreshold)
	}
	// A nil table has to answer the same way, since that is the state of every
	// config that never mentions the section.
	d := Defaults()
	if got := d.Thresholds().For(usage.WindowFiveHour); got != d.Threshold {
		t.Errorf("Defaults().Thresholds().For(five_hour) = %v, want %v; a nil table is not a missing answer", got, d.Threshold)
	}
}

// go-toml decodes `five_hour = inf` into a float64 without complaint. An
// infinite threshold makes that window's distance from its own floor infinite,
// so the one window the user tightened becomes the one that can never bind.
func TestAWindowThresholdIsHeldToTheSameRangeAsTheGlobalOne(t *testing.T) {
	for _, body := range []string{
		"[window_threshold]\nfive_hour = inf",
		"[window_threshold]\nfive_hour = -inf",
		"[window_threshold]\nfive_hour = nan",
		"[window_threshold]\nfive_hour = 0.0",
		"[window_threshold]\nfive_hour = -5.0",
		"[window_threshold]\nfive_hour = 101.0",
	} {
		cfg, err := Parse([]byte(body))
		if err == nil {
			t.Errorf("%q was accepted", body)
			continue
		}
		if !strings.Contains(err.Error(), "five_hour") {
			t.Errorf("error = %q, want it to name the window whose value was refused", err)
		}
		if !cfg.Equal(Config{}) {
			t.Errorf("Parse(%q) returned %+v alongside its error; a refused file must yield nothing usable", body, cfg)
		}
	}
}

func TestAnEmptyWindowThresholdTableIsTheSameAsNoTable(t *testing.T) {
	cfg, err := Parse([]byte("[window_threshold]\n"))
	if err != nil {
		t.Fatalf("Parse() = %v; an empty table is a legal document", err)
	}
	if cfg.WindowThreshold != nil {
		t.Errorf("WindowThreshold = %v, want nil — an empty table says every window uses `threshold`, which is what nil already says", cfg.WindowThreshold)
	}
	if !cfg.Equal(Defaults()) {
		t.Errorf("Parse() = %+v, want the defaults", cfg)
	}
}

// The reason the table is top-level and not `[threshold.window]`: `threshold`
// is already a scalar and TOML forbids reopening that name as a table. Pinned
// so the spelling nobody can have is not re-introduced.
func TestThresholdCannotBeReopenedAsATable(t *testing.T) {
	_, err := Parse([]byte("threshold = 80.0\n[threshold.window]\nfive_hour = 85\n"))
	if err == nil {
		t.Fatal("`threshold = 80` alongside [threshold.window] parsed; go-toml refuses a key that already exists as a value, which is why the per-window table needs its own top-level name")
	}
	if !strings.Contains(err.Error(), "threshold") {
		t.Errorf("error = %q, want it to name the key that collides", err)
	}
}

// preempt_lead is the one duration key whose ZERO is a value. The pre-emptive
// switch is an opt-out a user may legitimately take — they would rather never
// be moved early — where a zero cooldown is a switch storm and a zero
// hysteresis is no margin at all. The last case pins that the difference did
// not leak onto the anti-flap keys.
func TestAZeroPreemptLeadIsPreemptionOffAndANegativeOneIsRefused(t *testing.T) {
	cfg, err := Parse([]byte(`preempt_lead = "0s"`))
	if err != nil {
		t.Fatalf("Parse() = %v; zero is how a user switches the pre-emptive switch off", err)
	}
	if cfg.PreemptLead != 0 {
		t.Errorf("PreemptLead = %v, want 0", cfg.PreemptLead)
	}
	if _, err := Parse([]byte(`preempt_lead = "-2m"`)); err == nil {
		t.Error(`preempt_lead = "-2m" was accepted; a negative lead puts the switch after the exhaustion it exists to get ahead of`)
	}
	if _, err := Parse([]byte(`cooldown = "0s"`)); err == nil {
		t.Error("cooldown = 0s was accepted; the zero that means off for preempt_lead must not have leaked onto the anti-flap keys")
	}
}

// Equal replaced ==, which the compiler kept complete for free. Nothing keeps a
// hand-written comparison complete, and the symptom of a forgotten field is
// silent: two configs that differ report as the same one, so a reload decides
// there is nothing to pick up.
func TestEqualComparesEveryFieldOfConfig(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := range typ.NumField() {
		mutated := Defaults()
		f := reflect.ValueOf(&mutated).Elem().Field(i)
		name := typ.Field(i).Name
		switch f.Kind() {
		case reflect.Float64:
			f.SetFloat(f.Float() + 1)
		case reflect.Int64:
			f.SetInt(f.Int() + 1)
		case reflect.Uint8:
			f.SetUint(f.Uint() + 1)
		case reflect.Bool:
			f.SetBool(!f.Bool())
		case reflect.String:
			// A name key. The gate needs a DIFFERENT value, not a legal one --
			// Equal compares and never validates -- so appending is enough, and
			// stays right as names are added on either side.
			f.SetString(f.String() + "-changed")
		case reflect.Map:
			f.Set(reflect.ValueOf(map[usage.WindowName]float64{usage.WindowFiveHour: 85}))
		case reflect.Struct:
			// A nested table. Each of its fields is changed on its OWN copy
			// and asserted separately, because changing them all at once would
			// pass for an Equal that compares only the first: the symptom of a
			// forgotten field is silent either way, and a gate that cannot see
			// one field of a table is no gate on that table.
			for j := range f.NumField() {
				nested := Defaults()
				sub := reflect.ValueOf(&nested).Elem().Field(i).Field(j)
				subName := name + "." + f.Type().Field(j).Name
				switch sub.Kind() {
				case reflect.Float64:
					sub.SetFloat(sub.Float() + 1)
				case reflect.Int:
					sub.SetInt(sub.Int() + 1)
				case reflect.Bool:
					sub.SetBool(!sub.Bool())
				case reflect.String:
					sub.SetString(sub.String() + "-changed")
				default:
					t.Fatalf("Config.%s is a %s, which this gate cannot change on its own; teach it that kind before adding the field", subName, sub.Kind())
				}
				if Defaults().Equal(nested) {
					t.Errorf("Equal() reported two configs alike after changing %s; the field is missing from Equal", subName)
				}
			}
			continue
		default:
			t.Fatalf("Config.%s is a %s, which this gate cannot change on its own; teach it that kind before adding the field", name, f.Kind())
		}
		if Defaults().Equal(mutated) {
			t.Errorf("Equal() reported two configs alike after changing %s; the field is missing from Equal", name)
		}
	}
}

// A nil map and an empty one both say "every window uses Threshold". Reporting
// them as different configurations would have the daemon announce a config
// change nobody made.
func TestANilWindowThresholdEqualsAnEmptyOne(t *testing.T) {
	empty := Defaults()
	empty.WindowThreshold = map[usage.WindowName]float64{}
	if !Defaults().Equal(empty) {
		t.Error("Equal() separated a nil window table from an empty one; both mean every window uses `threshold`")
	}
}

// update_check is the only key in ccdad that governs a network call to a host
// other than api.anthropic.com, so its default and its off switch are both
// worth pinning: an air-gapped or egress-filtered machine has to be able to say
// "stop asking", and every other machine has to get the check without asking
// for it.
func TestTheUpdateCheckKeyDefaultsToOnAndCanBeSwitchedOff(t *testing.T) {
	if !Defaults().UpdateCheck {
		t.Error("Defaults().UpdateCheck = false; a user who has never edited the file would never hear that a release shipped")
	}
	cfg, err := Parse([]byte("update_check = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpdateCheck {
		t.Error("update_check = false did not reach the config; the one key that stops an egress-filtered machine calling out does nothing")
	}
	// Hover derives thresholds. This is not one: it is whether the daemon may
	// make a request at all, and a mode that supplied it would be deciding on
	// the user's behalf that fully automatic also means fully connected.
	if HoverOverrides(keyUpdateCheck) {
		t.Error("hover overrides update_check; a ranking policy must not switch a network call back on")
	}
}
