package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// clearCooldown removes the stamp a preceding setup switch left, so a test that
// is about the RANKING is not silently testing the cooldown instead.
func clearCooldown(t *testing.T) {
	t.Helper()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.RecordSwitch("", time.Time{})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// seedUsageAt is seedUsage with the reading's own timestamp, for the case the
// cache's Prune rule exists for: an entry left behind by a PREVIOUS account at
// the same uuid.
func seedUsageAt(t *testing.T, uuid string, headroom float64, fetchedAt time.Time) {
	t.Helper()
	pct := 100 - headroom
	resets := time.Now().Add(time.Hour)
	snap := &usage.Snapshot{FiveHour: usage.NewWindow(&pct, &resets)}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(uuid, usage.Entry{Snapshot: snap, FetchedAt: fetchedAt})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// seedWeekly puts a reading whose PERISHABLE seven-day window is what moves,
// which is the axis consume-first ranks on.
func seedWeekly(t *testing.T, uuid string, headroom float64, expiresIn time.Duration) {
	t.Helper()
	pct := 100 - headroom
	resets := time.Now().Add(expiresIn)
	snap := &usage.Snapshot{SevenDay: usage.NewWindow(&pct, &resets)}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(uuid, usage.Entry{Snapshot: snap, FetchedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A missing argument is a caller mistake, which the exit contract reserves 2
// for. Cobra reports it as a plain error, which would exit 1 and make a cron
// line that lost its account indistinguishable from a network failure.
func TestSwitchAndRemoveWithNoArgumentAreUsageErrors(t *testing.T) {
	isolate(t)

	for _, name := range []string{"switch", "remove"} {
		code, _, _, _ := runRoot(t, name)
		if code != ExitUsage {
			t.Errorf("bare %s = %d, want %d", name, code, ExitUsage)
		}
	}
}

func TestSwitchToAnUnknownAccountIsUsageError(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, _, _, top := runRoot(t, "switch", "nope")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "Available") {
		t.Fatalf("error %q should list the available references", top)
	}
}

// Switching to the account already live is exit 3, and it must not rewrite the
// live credentials file.
func TestSwitchToActiveAccountIsExitThree(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("first switch = %d (%s), want 0", code, top)
	}
	before, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}

	code, out, errOut, _ := runRoot(t, "switch", "1")
	if code != ExitNothingToDo {
		t.Fatalf("second switch = %d, want %d", code, ExitNothingToDo)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "Already on") {
		t.Fatalf("stderr = %q, want the no-op notice", errOut)
	}
	after, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("a no-op switch rewrote the live credentials file")
	}

	// --force is the documented escape hatch and must still activate.
	if code, _, _, top := runRoot(t, "switch", "1", "--force"); code != ExitOK {
		t.Fatalf("switch --force = %d (%s), want 0", code, top)
	}
}

// Claude Code reads a token account's credential from an environment variable,
// never from the credentials file, so there is nothing to install. Say that
// instead of handing cclink a snapshot it will refuse for a reason the user
// cannot act on.
func TestSwitchRefusesASetupTokenAccount(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, profileJSON("u-token", "token@example.com"))
	})
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-TESTTOKEN"); err != nil {
		t.Fatal(err)
	}

	code, _, _, top := runRoot(t, "switch", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("error %q should name the mechanism that does work", top)
	}
	assertNoLiveCredentials(t)
}

// An api-key account IS switchable, and the switch is two writes to two files.
//
// The credentials half is the one a plausible implementation omits, and the
// assertion for it is written against a file that HAS a login: Claude Code
// prefers claudeAiOauth over its stored primaryApiKey in every configuration,
// so a switch that writes only the config reports success while the session
// goes on using the old account. With an empty credentials file both
// implementations look identical.
func TestSwitchInstallsAnAPIKeyAccount(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	const key = "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"
	if err, _, _ := runCmd(t, newAddTokenCmd(), key); err != nil {
		t.Fatal(err)
	}
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"live","refreshToken":"live-r"},"mcpOAuth":{"srv":1}}`)

	code, _, _, top := runRoot(t, "switch", "1")
	if code != ExitOK {
		t.Fatalf("switch = %d (%s), want 0", code, top)
	}

	cfg, err := cclink.LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := cclink.PrimaryAPIKey(cfg); !ok || got != key {
		t.Fatalf("primaryApiKey = %q (present %v), want the switched-to key", got, ok)
	}
	live, err := cclink.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := live["claudeAiOauth"]; still {
		t.Fatal("the OAuth login survived the switch, so the stored key is inert and nothing changed")
	}
	if _, gone := live["mcpOAuth"]; !gone {
		t.Fatal("the switch destroyed the machine-scoped mcpOAuth key")
	}

	// The second switch is the already-on check, and it has to consider BOTH
	// halves: a check that only compared the stored key would also report
	// "already on" in the state where the key is stored but a login is still in
	// front of it — which is the state where nothing is in effect.
	code, _, _, _ = runRoot(t, "switch", "1")
	if code != ExitNothingToDo {
		t.Fatalf("second switch = %d, want %d (already on)", code, ExitNothingToDo)
	}
}

// Switching back to a login must take ccdad's key out of the config.
//
// Leaving it is not neutral. The login wins for as long as it is there, so
// nothing looks wrong — and the moment the user signs out of Claude Code, a key
// belonging to a DIFFERENT account silently becomes the credential, because
// primaryApiKey is exactly Claude Code's fallback for "no login".
func TestSwitchToALoginClearsCcdadsStoredAPIKey(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	const key = "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"
	if err, _, _ := runCmd(t, newAddTokenCmd(), key); err != nil {
		t.Fatal(err)
	}
	seedAccount(t, "u-1", "a@example.com")

	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch to the api key = %d (%s)", code, top)
	}
	if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
		t.Fatalf("switch to the login = %d (%s)", code, top)
	}

	cfg, err := cclink.LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := cclink.PrimaryAPIKey(cfg); ok {
		t.Fatalf("primaryApiKey = %q after switching to a login; it becomes the credential again on sign-out", got)
	}
	// The approval entry is left behind on purpose: it is twenty characters of
	// a key, it is consent the user gave through Claude Code's own prompt, and
	// removing it would make the next switch back prompt again.
	if approved := cclink.ApprovedAPIKeys(cfg); len(approved) != 1 {
		t.Fatalf("approved = %v, want the approval entry preserved", approved)
	}
}

// The unknown-key probe runs on every switch: drift here is demonstrated
// rather than hypothetical. Merge preserves what it does not recognize, but the
// operator still has to be told a new key exists.
func TestSwitchWarnsAboutUnknownKeysAndPreservesThem(t *testing.T) {
	claude := isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	live := `{"claudeAiOauth":{"accessToken":"OTHER","refreshToken":"OTHER-RT"},"somethingNew":{"a":1}}`
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, top := runRoot(t, "switch", "1")
	if code != ExitOK {
		t.Fatalf("switch = %d (%s), want 0", code, top)
	}
	if !strings.Contains(errOut, "somethingNew") {
		t.Fatalf("stderr = %q, want the unrecognized key named", errOut)
	}

	raw, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	if _, ok := after["somethingNew"]; !ok {
		t.Fatalf("the unrecognized key was dropped by the switch: %s", raw)
	}
	_ = claude
}

// An unreadable live file must not be what stops the switch: attribution is
// only the already-on optimization, so the user is told and the command
// carries on to the activation.
//
// The activation then refuses, and that is the right answer rather than a
// missing feature: cclink re-reads under the lock and will not merge into a
// file it cannot parse, because overwriting would destroy the machine-scoped
// keys — trustedDeviceToken, enterpriseGateway — that the damaged file still
// holds. What this pins is that the refusal comes from the activation, with a
// message naming the real problem, and not from the optional read above it.
func TestSwitchGetsPastAnUnreadableLiveFileToTheRealError(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, top := runRoot(t, "switch", "1")
	if !strings.Contains(errOut, "could not read the current login") {
		t.Fatalf("stderr = %q, want the situation named before the attempt", errOut)
	}
	if code == ExitOK {
		t.Fatal("switch reported success against a credentials file cclink cannot parse")
	}
	if !strings.Contains(top, "parsing credentials") {
		t.Fatalf("error %q should come from the activation, not from the attribution read", top)
	}
	s, _ := store.Open()
	if got := s.ActiveUUID(); got != "" {
		t.Fatalf("ActiveUUID() = %q, want it unset after a failed activation", got)
	}
}

// With CLAUDE_CODE_OAUTH_TOKEN set, Claude Code reads that token in preference
// to the credentials file, so a switch that rewrites the file changes nothing
// about what Claude Code uses. The command still does what it was asked, but
// silently having no effect is the failure mode worth naming.
func TestSwitchWarnsWhenAnEnvironmentTokenOverridesIt(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-OVERRIDE")

	code, _, errOut, top := runRoot(t, "switch", "1")
	if code != ExitOK {
		t.Fatalf("switch = %d (%s), want it to still do the work", code, top)
	}
	if !strings.Contains(errOut, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("stderr = %q, want the override named", errOut)
	}
}

// The already-on check must ask the FILE, not the environment. With an
// environment token set and the file already holding this account, asking the
// environment says "not current" and the switch pointlessly rewrites a file
// that already says the right thing.
func TestSwitchAlreadyOnAsksTheFileNotTheEnvironment(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("first switch = %d (%s)", code, top)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-UNRELATED")

	code, _, errOut, _ := runRoot(t, "switch", "1")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d, want %d: the file already holds this account", code, ExitNothingToDo)
	}
	if !strings.Contains(errOut, "Already on") {
		t.Fatalf("stderr = %q, want the no-op notice", errOut)
	}
}

// Nothing asserted ActiveUUID after a SUCCESSFUL switch; the only assertion
// nearby expects "" after a FAILED one, which never calling SetActive satisfies
// perfectly. The stored value is load-bearing: `ccdad which` and the re-auth key
// carry both read it.
func TestSwitchRecordsTheActiveAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")

	if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ActiveUUID(); got != "u-2" {
		t.Fatalf("ActiveUUID() = %q, want u-2", got)
	}
}

// seedUsage puts one reading in the on-disk cache, which is the only thing a
// targetless switch ranks on: `switch` never polls, for the same reason `list`
// does not.
func seedUsage(t *testing.T, uuid string, headroom float64) {
	t.Helper()
	pct := 100 - headroom
	resets := time.Now().Add(time.Hour)
	snap := &usage.Snapshot{FiveHour: usage.NewWindow(&pct, &resets)}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(uuid, usage.Entry{Snapshot: snap, FetchedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func liveUUIDOf(t *testing.T) string {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	live, err := cclink.Load()
	if err != nil {
		t.Fatal(err)
	}
	acct, ok := switcher.AttributeFile(live, s.Accounts(), s.Credentials)
	if !ok {
		return ""
	}
	return acct.UUID
}

// ---- the three grammars ----------------------------------------------------

// The targetless form. The engine picks under the anti-flap margins and
// installs the winner.
func TestSwitchWithNoTargetLetsTheEngineChoose(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 10)
	seedUsage(t, "u-2", 80)
	clearCooldown(t)

	code, _, errOut, top := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if got := liveUUIDOf(t); got != "u-2" {
		t.Fatalf("live account = %q, want u-2 — the one with headroom", got)
	}
}

// The engine must obey the strategy it was given, not just the default.
func TestSwitchHonoursTheNamedStrategy(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	// u-2 has far more headroom; u-1's weekly quota is the one about to expire.
	seedWeekly(t, "u-1", 40, 12*time.Hour)
	seedWeekly(t, "u-2", 95, 6*24*time.Hour)
	clearCooldown(t)

	if code, _, errOut, top := runRoot(t, "switch", "--strategy", "headroom"); code != ExitOK {
		t.Fatalf("headroom exit = %d (%s / %s)", code, errOut, top)
	}
	if got := liveUUIDOf(t); got != "u-2" {
		t.Fatalf("headroom chose %q, want u-2", got)
	}

	clearCooldown(t)
	if code, _, errOut, top := runRoot(t, "switch", "--strategy", "consume-first"); code != ExitOK {
		t.Fatalf("consume-first exit = %d (%s / %s)", code, errOut, top)
	}
	if got := liveUUIDOf(t); got != "u-1" {
		t.Fatalf("consume-first chose %q, want u-1 — perishable quota is spent first", got)
	}
}

// The two rejections are asymmetric and easy to invert: --strategy is refused
// WITH a target, --model WITHOUT --strategy.
func TestSwitchRejectsTheTwoBadCombinations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		mention string
	}{
		{"strategy with an explicit target", []string{"switch", "1", "--strategy", "headroom"}, "cannot be given one as well"},
		{"model without a strategy", []string{"switch", "--model", "opus"}, "alongside --strategy"},
		{"model with an explicit target", []string{"switch", "1", "--model", "opus"}, "alongside --strategy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedAccount(t, "u-1", "a@example.com")

			code, _, _, top := runRoot(t, tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d", code, ExitUsage)
			}
			if !strings.Contains(top, tc.mention) {
				t.Fatalf("error %q should say %q", top, tc.mention)
			}
			assertNoLiveCredentials(t)
		})
	}
}

// seedModelWindows puts a reading whose per-model weekly caps are what differ,
// which is the axis --model narrows.
func seedModelWindows(t *testing.T, uuid string, fiveHour, opus, sonnet float64) {
	t.Helper()
	resets := time.Now().Add(48 * time.Hour)
	win := func(used float64) usage.Window { return usage.NewWindow(&used, &resets) }
	snap := &usage.Snapshot{
		FiveHour:       win(fiveHour),
		SevenDayOpus:   win(opus),
		SevenDaySonnet: win(sonnet),
	}
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(uuid, usage.Entry{Snapshot: snap, FetchedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --model narrows the ranking to the caps that bind for the model this
// session will run. The control is the same fixture without the flag, which
// picks the OTHER account — so this cannot pass by the flag being ignored.
func TestSwitchModelNarrowsTheRanking(t *testing.T) {
	setup := func(t *testing.T) {
		isolate(t)
		seedAccount(t, "u-1", "a@example.com")
		seedAccount(t, "u-2", "b@example.com")
		seedAccount(t, "u-3", "c@example.com")
		if code, _, _, top := runRoot(t, "switch", "3"); code != ExitOK {
			t.Fatalf("setup switch = %d (%s)", code, top)
		}
		// u-1's Opus week is gone and its Sonnet week is barely touched; u-2 is
		// middling everywhere. u-3 is the live account and the worst of the
		// three, so it is never the answer.
		seedModelWindows(t, "u-1", 10, 99, 5)
		seedModelWindows(t, "u-2", 50, 50, 40)
		seedModelWindows(t, "u-3", 95, 95, 95)
		clearCooldown(t)
	}

	t.Run("unqualified", func(t *testing.T) {
		setup(t)
		if code, _, errOut, top := runRoot(t, "switch", "--strategy", "headroom"); code != ExitOK {
			t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
		}
		if got := liveUUIDOf(t); got != "u-2" {
			t.Fatalf("chose %q, want u-2 — u-1's spent Opus week binds when no model is named", got)
		}
	})

	t.Run("--model sonnet", func(t *testing.T) {
		setup(t)
		if code, _, errOut, top := runRoot(t, "switch", "--strategy", "headroom", "--model", "sonnet"); code != ExitOK {
			t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
		}
		if got := liveUUIDOf(t); got != "u-1" {
			t.Fatalf("chose %q, want u-1 — a spent Opus week does not bind a Sonnet session", got)
		}
	})
}

// A model name ccdad cannot place narrows nothing, so honouring it silently
// would rank exactly as if the flag had not been typed while the user believed
// they had excluded another model's spent cap.
func TestSwitchRefusesAModelItCannotPlace(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedUsage(t, "u-1", 90)

	code, _, _, top := runRoot(t, "switch", "--strategy", "headroom", "--model", "gpt-5")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	for _, want := range []string{"gpt-5", "opus", "sonnet"} {
		if !strings.Contains(top, want) {
			t.Errorf("error %q should mention %q", top, want)
		}
	}
	assertNoLiveCredentials(t)
}

// The message a user sees most often. "needs exactly one account" was false the
// moment the strategy form existed.
func TestSwitchWithNoArgumentNamesBothGrammars(t *testing.T) {
	isolate(t)

	code, _, _, top := runRoot(t, "switch")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "--strategy") {
		t.Errorf("error %q should name the targetless grammar", top)
	}
	if strings.Contains(top, "exactly one") {
		t.Errorf("error %q still claims switch takes exactly one account", top)
	}
}

func TestSwitchWithTwoAccountsIsUsageError(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")

	code, _, _, top := runRoot(t, "switch", "1", "2")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "at most one account") {
		t.Fatalf("error %q should say at most one", top)
	}
}

// A typo'd strategy is a usage error, never a silent run of the default. That
// is the exit contract's whole complaint about cswap.
func TestSwitchRefusesAnUnknownStrategy(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	code, _, _, top := runRoot(t, "switch", "--strategy", "most-headroom")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "consume-first") {
		t.Fatalf("error %q should list the strategies that do exist", top)
	}
	assertNoLiveCredentials(t)
}

// ---- the anti-flap state, across processes ---------------------------------

// A one-shot command has no in-memory anti-flap history. Reading the cooldown
// from disk is what stops a bare `switch` ping-ponging against a daemon that is
// honouring it.
func TestATargetlessSwitchReadsTheCooldownFromDisk(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 10)
	seedUsage(t, "u-2", 80)

	// The setup switch stamped the cooldown itself, which is the point.
	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitNothingToDo)
	}
	if !strings.Contains(errOut, "too recently") {
		t.Errorf("stderr = %q, want the cooldown named", errOut)
	}
	// A hold with no end in sight is indistinguishable from a refusal.
	if !strings.Contains(errOut, "try again after") {
		t.Errorf("stderr = %q, want the moment the cooldown lifts", errOut)
	}
	if got := liveUUIDOf(t); got != "u-1" {
		t.Fatalf("live account = %q; the cooldown was ignored", got)
	}

	// --force is the explicit bypass.
	if code, _, errOut, top := runRoot(t, "switch", "--strategy", "headroom", "--force"); code != ExitOK {
		t.Fatalf("forced exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if got := liveUUIDOf(t); got != "u-2" {
		t.Fatalf("live account = %q after --force, want u-2", got)
	}
}

// An EXPLICIT switch stamps the cooldown too: the user has just chosen an
// account and a daemon evaluating ten seconds later must not override it.
func TestAnExplicitSwitchStampsTheCooldown(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}

	st, err := strategy.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	last, to := st.LastSwitch()
	if last.IsZero() || to != "u-1" {
		t.Fatalf("LastSwitch = %v/%q, want a stamp naming u-1", last, to)
	}
	if _, cooling := st.CooldownRemaining(time.Now(), strategy.DefaultCooldown); !cooling {
		t.Error("the cooldown is not in force right after a switch")
	}
}

// A quarantined account is out of auto-rotation, and with nothing else to move
// to that is exit 4 — actionable — rather than exit 3.
func TestATargetlessSwitchWithNoViableTargetIsBlocked(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 10)
	seedUsage(t, "u-2", 80)
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.RecordSwitch("", time.Time{})
		st.Quarantine("u-1", time.Now(), time.Hour, "dead refresh token")
		st.Quarantine("u-2", time.Now(), time.Hour, "dead refresh token")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitBlocked {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitBlocked)
	}
	// The count and the remedy, not just the word: a user whose engine is
	// parked has to be told that re-authenticating is what unparks it.
	if !strings.Contains(errOut, "2 account(s) quarantined") || !strings.Contains(errOut, "ccdad add") {
		t.Errorf("stderr = %q, want the count and the remedy", errOut)
	}
}

// With no readings the ranking has nothing to order on, so a move would be a
// reshuffle rather than a choice.
func TestATargetlessSwitchWithNoReadingsIsBlocked(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")

	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitBlocked {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitBlocked)
	}
	if !strings.Contains(errOut, "no usage readings") {
		t.Errorf("stderr = %q, want the missing readings named", errOut)
	}
	assertNoLiveCredentials(t)
}

// Being already on the best account is exit 3, and it must not rewrite the file.
func TestATargetlessSwitchOnTheBestAccountIsExitThree(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 80)
	seedUsage(t, "u-2", 10)
	clearCooldown(t)
	before, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}

	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitNothingToDo)
	}
	after, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("a no-op targetless switch rewrote the live credentials file")
	}
}

// Hysteresis has to measure against the account in the LIVE CREDENTIALS FILE.
// store.ActiveUUID is a display hint that goes stale the moment the user runs
// /login inside Claude Code, and a margin measured against it compares the
// candidate to an account that is not there.
//
// The two baselines are made to give different EXIT CODES, not just different
// reasons: an earlier version of this test asserted exit 3 and passed with
// either baseline, because the wrong one happened to fail hysteresis.
func TestATargetlessSwitchMeasuresAgainstTheLiveFileNotTheStoreHint(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	seedAccount(t, "u-3", "c@example.com")
	// ccdad last activated u-2 and recorded it in accounts.toml, then something
	// outside ccdad put u-1 back in the credentials file.
	if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-u-1"}}`)
	// u-3 tops the ranking either way. Against the live u-1 it fails the 2.0
	// ratio and nothing happens; against the stale hint u-2 it clears every
	// margin and the engine moves the user off a login it never measured.
	seedUsage(t, "u-3", 100)
	seedUsage(t, "u-1", 60)
	seedUsage(t, "u-2", 20)
	clearCooldown(t)

	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d (%s), want %d — u-3 does not have twice the live account's headroom", code, errOut, ExitNothingToDo)
	}
	if got := liveUUIDOf(t); got != "u-1" {
		t.Fatalf("live account = %q, want u-1 left alone", got)
	}
}

// A token account cannot become the live login, so leaving it in the ranking
// would turn a strategy the user asked for into an exit 2 they cannot act on.
//
// It has to be a SETUP TOKEN. An api-key account is already held out by
// identity.KindAPIKey, so testing with one proves nothing about this rule — a
// setup token classifies as a subscription and would rank.
func TestATargetlessSwitchNeverRanksATokenAccount(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("u-tok", "tok@example.com"))
	})
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-TESTTOKEN"); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := s.Get("u-tok")
	if !ok || tok.Kind == identity.KindAPIKey {
		t.Fatalf("setup: the setup-token account is %+v; it must be rankable to be a real test", tok)
	}
	// The token account looks like the best target on every axis.
	seedUsage(t, "u-1", 10)
	seedUsage(t, "u-2", 20)
	seedUsage(t, "u-tok", 99)
	clearCooldown(t)

	code, _, errOut, top := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0", code, errOut, top)
	}
	if got := liveUUIDOf(t); got != "u-2" {
		t.Fatalf("live account = %q, want u-2 — the best account that can actually be installed", got)
	}
}

// A reading OLDER than the account's AddedAt belonged to a previous account at
// the same uuid, removed and added again. Letting it through hands a fresh
// login the headroom its predecessor had already spent.
func TestATargetlessSwitchIgnoresAReadingFromAPreviousAccount(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 30)
	seedUsageAt(t, "u-2", 90, time.Now().Add(-time.Hour))
	clearCooldown(t)

	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d (%s), want %d — u-2's reading predates the account", code, errOut, ExitNothingToDo)
	}
	if got := liveUUIDOf(t); got != "u-1" {
		t.Fatalf("live account = %q, want u-1", got)
	}
}

// --force overrides the anti-flap HOLD, not the ranking. With nothing to move
// to there is no hold to override, so it stays a no-op.
func TestForceOnTheBestAccountIsStillNothingToDo(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "u-1", 80)
	seedUsage(t, "u-2", 10)
	before, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}

	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom", "--force")
	if code != ExitNothingToDo {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitNothingToDo)
	}
	after, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("--force rewrote the live credentials file with nothing to switch to")
	}
}

// --force must never reach the credit pool. The credit gate requires two
// independent opt-ins before ccdad spends money, and a flag named "force" is
// neither of them.
func TestForceNeverReachesTheCreditPool(t *testing.T) {
	isolate(t)
	seedCreditAccount(t, "c-1", "one@example.com")
	seedCreditAccount(t, "c-2", "two@example.com")
	if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
		t.Fatalf("setup switch = %d (%s)", code, top)
	}
	seedUsage(t, "c-1", 90)
	seedUsage(t, "c-2", 5)
	clearCooldown(t)

	code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom", "--force")
	if code == ExitOK {
		t.Fatalf("exit = 0 (%s); --force moved into the credit pool", errOut)
	}
	if got := liveUUIDOf(t); got != "c-2" {
		t.Fatalf("live account = %q, want c-2 untouched", got)
	}
}

// seedCreditReading puts a reading carrying the CREDIT axis, which is what the
// gate prices a switch on. Wire amounts are in the currency's minor unit.
func seedCreditReading(t *testing.T, uuid string, limitCents, usedCents float64) {
	t.Helper()
	e := usage.ExtraUsageFor(usage.ExtraUsageInput{
		State: usage.ExtraUsageEnabled, Currency: "USD",
		MonthlyLimit: &limitCents, UsedCredits: &usedCents,
	})
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put(uuid, usage.Entry{Snapshot: &usage.Snapshot{ExtraUsage: e}, FetchedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// "the credit gate refused" tells nobody whether to raise max_auto_spend or to
// call their organization. The gate's own reason is the actionable half.
func TestATargetlessSwitchNamesWhyTheCreditGateRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seed    func(t *testing.T)
		mention string
	}{
		{"never opted in", func(t *testing.T) { seedCreditReading(t, "c-1", 10000, 0) }, "max_auto_spend is 0"},
		{"spend cannot be read", func(t *testing.T) { seedUsage(t, "c-1", 50) }, "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedAccount(t, "u-1", "a@example.com")
			seedCreditAccount(t, "c-1", "money@example.com")
			if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
				t.Fatalf("setup switch = %d (%s)", code, top)
			}
			// The one subscription account is spent, which is the only thing
			// that opens step 2 of the credit gate.
			seedUsage(t, "u-1", 5)
			tc.seed(t)
			clearCooldown(t)

			code, _, errOut, _ := runRoot(t, "switch", "--strategy", "headroom")
			if code != ExitBlocked {
				t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitBlocked)
			}
			if !strings.Contains(errOut, tc.mention) {
				t.Errorf("stderr = %q, want the gate's own reason %q", errOut, tc.mention)
			}
			if got := liveUUIDOf(t); got != "u-1" {
				t.Fatalf("live account = %q; ccdad spent money it was never opted in to", got)
			}
		})
	}
}

// The two writes happen config-first, and the order is a safety property rather
// than a style choice.
//
// The key is inert while a login sits in front of it, so writing the config
// first means an interruption between the two leaves the machine logged in as
// whatever it was, with an unused key beside it. The opposite order leaves a
// window with no login and no key: a logged-out machine, from a command that
// was asked to switch accounts.
//
// The failure is induced by making ~/.claude.json unreadable rather than by a
// seam, so the test drives the real code path a full disk or a permission
// change would.
func TestSwitchToAnAPIKeyLeavesTheLoginAloneWhenTheConfigCannotBeWritten(t *testing.T) {
	claude := isolate(t)
	stubEnvironment(t, false, false)
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"); err != nil {
		t.Fatal(err)
	}
	const login = `{"claudeAiOauth":{"accessToken":"live","refreshToken":"live-r"}}`
	writeLiveFile(t, login)

	// A directory where the config file goes: readable as a path, unreadable as
	// a document, on every platform.
	if err := os.MkdirAll(filepath.Join(claude, ".claude.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	if code, _, _, _ := runRoot(t, "switch", "1"); code == ExitOK {
		t.Fatal("switch reported success with the config unwritable")
	}

	raw, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	if _, ok := after["claudeAiOauth"]; !ok {
		t.Fatalf("the login was removed before the key was stored, so the machine is signed out of everything: %s", raw)
	}
}

// A primaryApiKey ccdad did not install is left where it is.
//
// It came from Claude Code's own `/login`, it is inert for as long as the login
// being installed is live, and deleting a credential ccdad never created is not
// a side effect a switch is entitled to have.
func TestSwitchToALoginLeavesAForeignAPIKeyAlone(t *testing.T) {
	claude := isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	if err := os.WriteFile(filepath.Join(claude, ".claude.json"),
		[]byte(`{"primaryApiKey":"sk-ant-api03-SOMEONE-ELSES"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, _, _, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}

	cfg, err := cclink.LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := cclink.PrimaryAPIKey(cfg); !ok || got != "sk-ant-api03-SOMEONE-ELSES" {
		t.Fatalf("primaryApiKey = %q (present %v); a key ccdad never stored must survive a switch", got, ok)
	}
}

// Removing an UNMANAGED login has to say so.
//
// A switch between two OAuth accounts destroys an unmanaged live login in the
// same way, so this is not a new hazard — but that switch leaves the machine
// holding another login, and this one leaves it holding none. The message
// ccdad prints for a managed login says the account can be switched back to,
// and printing that sentence for a login nothing has a copy of would be false.
func TestSwitchToAnAPIKeyWarnsWhenItRemovesAnUnmanagedLogin(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"); err != nil {
		t.Fatal(err)
	}
	// A login ccdad has never stored.
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"STRANGER","refreshToken":"RT-STRANGER"}}`)

	code, _, errOut, top := runRoot(t, "switch", "1")
	if code != ExitOK {
		t.Fatalf("switch = %d (%s)", code, top)
	}
	if !strings.Contains(errOut, "NOT one ccdad manages") {
		t.Fatalf("stderr = %q, want a warning that the removed login cannot be restored", errOut)
	}

	// The managed case must NOT carry that warning, or the warning means
	// nothing: a message printed either way tells a reader nothing about which
	// case they are in.
	isolate(t)
	stubEnvironment(t, false, false)
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"); err != nil {
		t.Fatal(err)
	}
	seedAccount(t, "u-1", "a@example.com")
	if code, _, _, top := runRoot(t, "switch", "2"); code != ExitOK {
		t.Fatalf("switch to the managed login = %d (%s)", code, top)
	}
	if code, _, errOut, top := runRoot(t, "switch", "1"); code != ExitOK {
		t.Fatalf("switch back to the api key = %d (%s)", code, top)
	} else if strings.Contains(errOut, "NOT one ccdad manages") {
		t.Fatalf("stderr = %q, want no warning for a login ccdad can restore", errOut)
	}
}
