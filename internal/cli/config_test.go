package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/config"
)

func configPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("CCDAD_HOME"), config.FileName)
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll(os.Getenv("CCDAD_HOME"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(t), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigNeedsASubcommand(t *testing.T) {
	isolate(t)

	code, _, _, top := runRoot(t, "config")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "get") {
		t.Errorf("stderr = %q, want the subcommands listed", top)
	}
}

func TestConfigPathReportsWhereTheStoreResolvedTo(t *testing.T) {
	isolate(t)

	code, stdout, _, _ := runRoot(t, "config", "path")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(stdout) != configPath(t) {
		t.Errorf("stdout = %q, want %q", strings.TrimSpace(stdout), configPath(t))
	}
}

func TestConfigPathAnswersInJSON(t *testing.T) {
	isolate(t)

	code, stdout, _, _ := runRoot(t, "config", "path", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var payload struct {
		SchemaVersion int    `json:"schemaVersion"`
		Path          string `json:"path"`
		Home          string `json:"home"`
		Exists        bool   `json:"exists"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if payload.SchemaVersion != 1 || payload.Path != configPath(t) || payload.Home != os.Getenv("CCDAD_HOME") {
		t.Errorf("payload = %+v", payload)
	}
	if payload.Exists {
		t.Error("exists = true with no config file written")
	}
}

// §9.3: a negative answer to a probe is 5, not 1 and not 2.
func TestGettingAnUnsetKeyIsTheProbeNegative(t *testing.T) {
	isolate(t)

	code, stdout, stderr, _ := runRoot(t, "config", "get", "threshold")
	if code != ExitProbeNegative {
		t.Errorf("exit = %d, want %d", code, ExitProbeNegative)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want nothing: the key is not set", stdout)
	}
	if !strings.Contains(stderr, "80") {
		t.Errorf("stderr = %q, want the default it would run on", stderr)
	}
}

func TestGettingAnUnsetKeyStillEmitsOneJSONObject(t *testing.T) {
	isolate(t)

	code, stdout, _, _ := runRoot(t, "config", "get", "threshold", "--json")
	if code != ExitProbeNegative {
		t.Errorf("exit = %d, want %d", code, ExitProbeNegative)
	}
	var payload struct {
		SchemaVersion int    `json:"schemaVersion"`
		Key           string `json:"key"`
		Value         string `json:"value"`
		Set           bool   `json:"set"`
		Source        string `json:"source"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if payload.Key != "threshold" || payload.Set || payload.Value != "80" || payload.Source != "default" {
		t.Errorf("payload = %+v", payload)
	}
}

func TestGettingAnUnknownKeyIsAUsageError(t *testing.T) {
	isolate(t)

	code, _, _, top := runRoot(t, "config", "get", "threshhold")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d: an unknown key is a typo, not a negative probe", code, ExitUsage)
	}
	if !strings.Contains(top, "threshold") {
		t.Errorf("stderr = %q, want the real keys listed", top)
	}
}

func TestSetThenGetRoundTripsAValue(t *testing.T) {
	isolate(t)

	if code, _, _, top := runRoot(t, "config", "set", "threshold", "90"); code != ExitOK {
		t.Fatalf("set exit = %d (%s)", code, top)
	}
	code, stdout, _, _ := runRoot(t, "config", "get", "threshold")
	if code != ExitOK {
		t.Fatalf("get exit = %d", code)
	}
	if strings.TrimSpace(stdout) != "90" {
		t.Errorf("stdout = %q, want 90", stdout)
	}
}

// An accepted typo is a config that silently does nothing, which is §9.3's
// whole argument for keeping 2 usage-only.
func TestSettingAnUnknownKeyIsAUsageError(t *testing.T) {
	isolate(t)

	code, _, _, top := runRoot(t, "config", "set", "threshhold", "90")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "threshold") {
		t.Errorf("stderr = %q, want the real keys listed", top)
	}
	if _, err := os.Stat(configPath(t)); !os.IsNotExist(err) {
		t.Error("a refused set created the config file")
	}
}

// Nothing token-shaped is settable: this file is the one people paste into an
// issue, and credentials live in the 0600 credential store instead.
func TestNoSecretShapedKeyIsSettable(t *testing.T) {
	isolate(t)

	for _, key := range []string{"token", "api_key", "oauth_token", "credit.token"} {
		if code, _, _, _ := runRoot(t, "config", "set", key, "sk-live-1"); code != ExitUsage {
			t.Errorf("set %s exit = %d, want %d", key, code, ExitUsage)
		}
	}
}

func TestSettingAnUnusableValueIsAUsageError(t *testing.T) {
	isolate(t)

	for _, tc := range [][2]string{
		{"threshold", "ninety"},
		{"threshold", "0"},
		{"cooldown", "300"},
		{"credit.max_auto_spend", "inf"},
		{"credit.max_auto_spend", "nan"},
		{"credit.max_auto_spend", "-1"},
		{"strategy", "consume-frist"},
	} {
		code, _, _, _ := runRoot(t, "config", "set", tc[0], tc[1])
		if code != ExitUsage {
			t.Errorf("set %s %s exit = %d, want %d", tc[0], tc[1], code, ExitUsage)
		}
	}
}

func TestUnsettingReportsNothingToDoTheSecondTime(t *testing.T) {
	isolate(t)

	if code, _, _, _ := runRoot(t, "config", "set", "threshold", "90"); code != ExitOK {
		t.Fatalf("set exit = %d", code)
	}
	if code, _, _, _ := runRoot(t, "config", "unset", "threshold"); code != ExitOK {
		t.Fatalf("first unset exit = %d, want 0", code)
	}
	if code, _, _, _ := runRoot(t, "config", "unset", "threshold"); code != ExitNothingToDo {
		t.Errorf("second unset exit = %d, want %d", code, ExitNothingToDo)
	}
}

func TestUnsettingAnUnknownKeyIsAUsageError(t *testing.T) {
	isolate(t)

	if code, _, _, _ := runRoot(t, "config", "unset", "threshhold"); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}

// §4.2 rule 3's shape: a key a newer ccdad wrote must survive an older one's
// write, or an upgrade silently deletes settings.
func TestAKeyThisReleaseDoesNotKnowSurvivesASet(t *testing.T) {
	isolate(t)
	writeConfig(t, "future_knob = 3\n\n[future]\nkeep = \"me\"\n")

	if code, _, _, top := runRoot(t, "config", "set", "threshold", "90"); code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, top)
	}
	raw, err := os.ReadFile(configPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"future_knob", "[future]", "keep"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the rewrite dropped %q:\n%s", want, raw)
		}
	}
}

func TestTheConfigFileIsWrittenPrivately(t *testing.T) {
	isolate(t)

	if code, _, _, _ := runRoot(t, "config", "set", "threshold", "90"); code != ExitOK {
		t.Fatal("set failed")
	}
	info, err := os.Stat(configPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600", perm)
	}
}

func TestListShowsEveryKeyAndWhereItsValueCameFrom(t *testing.T) {
	isolate(t)
	writeConfig(t, "threshold = 55\n")

	code, stdout, _, _ := runRoot(t, "config", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, key := range config.Keys() {
		if !strings.Contains(stdout, key) {
			t.Errorf("listing omits %q:\n%s", key, stdout)
		}
	}
	if !strings.Contains(stdout, "55") {
		t.Errorf("listing does not show the configured value:\n%s", stdout)
	}
	if !strings.Contains(stdout, "default") {
		t.Errorf("listing does not mark the keys that are defaults:\n%s", stdout)
	}
}

func TestListAnswersInJSON(t *testing.T) {
	isolate(t)
	writeConfig(t, "threshold = 55\nfuture_knob = 1\n")

	code, stdout, stderr, _ := runRoot(t, "config", "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var payload struct {
		SchemaVersion int    `json:"schemaVersion"`
		Path          string `json:"path"`
		Keys          []struct {
			Key    string `json:"key"`
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"keys"`
		UnknownKeys []string `json:"unknownKeys"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	if payload.SchemaVersion != 1 || len(payload.Keys) != len(config.Keys()) {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Keys[0].Key != "threshold" || payload.Keys[0].Value != "55" || payload.Keys[0].Source != "file" {
		t.Errorf("first key = %+v, want threshold 55 from the file", payload.Keys[0])
	}
	if len(payload.UnknownKeys) != 1 || payload.UnknownKeys[0] != "future_knob" {
		t.Errorf("unknownKeys = %v, want [future_knob]", payload.UnknownKeys)
	}
	if strings.Contains(stderr, "future_knob") == false {
		t.Errorf("stderr = %q, want the ignored key called out as a human notice too", stderr)
	}
}

// A broken file is a runtime failure, not a usage error: the caller's command
// was well formed and the file on disk is not.
func TestAnUnparseableFileIsAFailureRatherThanAUsageError(t *testing.T) {
	isolate(t)
	writeConfig(t, "threshold = = 90\n")

	for _, args := range [][]string{
		{"config", "list"},
		{"config", "get", "threshold"},
		{"config", "set", "threshold", "90"},
	} {
		if code, _, _, _ := runRoot(t, args...); code != ExitFailure {
			t.Errorf("%v exit = %d, want %d", args, code, ExitFailure)
		}
	}
	raw, err := os.ReadFile(configPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "threshold = = 90\n" {
		t.Errorf("the broken file was rewritten: %q", raw)
	}
}

// A value that is legal on its own but leaves the file as a whole unusable must
// not pass silently: the engine ignores the whole document, so the user would
// have set a key that changes nothing.
func TestSettingIntoAFileThatStaysInvalidWarnsLoudly(t *testing.T) {
	isolate(t)
	writeConfig(t, "hysteresis_pct = 0\n")

	code, _, stderr, _ := runRoot(t, "config", "set", "threshold", "90")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr, "hysteresis_pct") {
		t.Errorf("stderr = %q, want the problem that is still in the file", stderr)
	}
}

func TestGetTakesExactlyOneKey(t *testing.T) {
	isolate(t)

	for _, args := range [][]string{
		{"config", "get"},
		{"config", "get", "threshold", "extra"},
		{"config", "set", "threshold"},
		{"config", "unset"},
	} {
		if code, _, _, _ := runRoot(t, args...); code != ExitUsage {
			t.Errorf("%v exit = %d, want %d", args, code, ExitUsage)
		}
	}
}

// The engine has to actually read this file, or `ccdad config set` is a text
// editor with extra steps. max_auto_spend is the sharpest proof available: the
// default of 0 blocks the move with exit 4, and nothing but this file can raise
// it — §7.3 refuses to take a flag for it.
func TestATargetlessSwitchSpendsOnlyWhatTheConfigFileAllows(t *testing.T) {
	setup := func(t *testing.T) {
		t.Helper()
		isolate(t)
		seedAccount(t, "u-1", "a@example.com")
		seedCreditAccount(t, "c-1", "money@example.com")
		if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
			t.Fatalf("setup switch = %d (%s)", code, top)
		}
		seedUsage(t, "u-1", 5) // the subscription pool is spent
		seedCreditReading(t, "c-1", 10000, 0)
		clearCooldown(t)
	}

	t.Run("with no config file the gate refuses", func(t *testing.T) {
		setup(t)
		code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
		if code != ExitBlocked {
			t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitBlocked)
		}
	})

	t.Run("with a ceiling in the file the gate arms", func(t *testing.T) {
		setup(t)
		if code, _, _, top := runRoot(t, "config", "set", "credit.max_auto_spend", "100"); code != ExitOK {
			t.Fatalf("config set = %d (%s)", code, top)
		}
		code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
		if code != ExitOK {
			t.Fatalf("exit = %d (%s), want 0: max_auto_spend was raised to 100", code, errOut)
		}
		if got := liveUUIDOf(t); got != "c-1" {
			t.Errorf("live account = %q, want c-1", got)
		}
	})
}

// A broken config file must not stop a switch: the engine falls back to the
// defaults and says so, exactly as the daemon does.
func TestATargetlessSwitchWarnsAboutABrokenConfigAndKeepsGoing(t *testing.T) {
	isolate(t)
	writeConfig(t, "threshold = = 90\n")
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 10)
	seedUsage(t, "u-2", 90)
	clearCooldown(t)

	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want the switch to happen on the defaults", code, errOut)
	}
	if !strings.Contains(errOut, config.FileName) {
		t.Errorf("stderr = %q, want a note naming the file it could not read", errOut)
	}
}

// A duration is stored canonically, so the echo has to be the stored value and
// not the typed one — otherwise `set cooldown 300s` reports 300s and the very
// next `get cooldown` says 5m0s.
func TestSetEchoesTheValueItActuallyStored(t *testing.T) {
	isolate(t)

	code, _, stderr, _ := runRoot(t, "config", "set", "cooldown", "300s")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr, "5m0s") {
		t.Errorf("stderr = %q, want the canonical 5m0s it stored", stderr)
	}
	_, stdout, _, _ := runRoot(t, "config", "get", "cooldown")
	if strings.TrimSpace(stdout) != "5m0s" {
		t.Errorf("get = %q, want 5m0s", strings.TrimSpace(stdout))
	}
}

// The threshold decides whether the subscription pool counts as EXHAUSTED,
// which is the input that opens §7.3's credit gate — so a threshold read from
// the file changes what a targetless switch does, and one that is ignored does
// not.
func TestTheConfiguredThresholdReachesTheRankingPass(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedCreditAccount(t, "c-1", "money@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	// 60% used: spent under a threshold of 50, and not spent under the default
	// of 80.
	seedUsage(t, "u-1", 40)
	seedCreditReading(t, "c-1", 10000, 0)
	clearCooldown(t)
	writeConfig(t, "threshold = 50\n\n[credit]\nmax_auto_spend = 100\n")

	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0: at threshold 50 the subscription pool is spent", code, errOut)
	}
	if got := liveUUIDOf(t); got != "c-1" {
		t.Errorf("live account = %q, want c-1", got)
	}
}

// A file that DECODES but does not validate is ignored wholesale by the engine.
// A listing that showed the file's values would tell the user their setting is
// in force when nothing of the sort is happening.
func TestListShowsTheDefaultsWhenTheFileCannotBeUsed(t *testing.T) {
	isolate(t)
	writeConfig(t, "hysteresis_pct = 0\nthreshold = 55\n")

	code, stdout, stderr, _ := runRoot(t, "config", "list")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr, "hysteresis_pct") {
		t.Errorf("stderr = %q, want the value that made the file unusable", stderr)
	}
	if strings.Contains(stdout, "55") {
		t.Errorf("the listing shows a value the engine is not using:\n%s", stdout)
	}
	if !strings.Contains(stdout, "80") {
		t.Errorf("the listing does not show the default the engine IS using:\n%s", stdout)
	}
}
