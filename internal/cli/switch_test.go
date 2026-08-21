package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
)

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
	before, err := os.ReadFile(ccpath.CredentialsPath())
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
	after, err := os.ReadFile(ccpath.CredentialsPath())
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
func TestSwitchRefusesATokenAccount(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-TESTKEY"); err != nil {
		t.Fatal(err)
	}

	code, _, _, top := runRoot(t, "switch", "1")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(top, "ANTHROPIC_API_KEY") {
		t.Fatalf("error %q should name the mechanism that does work", top)
	}
	assertNoLiveCredentials(t)
}

// Spec §4.3 requires the unknown-key probe on every switch: drift here is
// demonstrated rather than hypothetical. Merge preserves what it does not
// recognize, but the operator still has to be told a new key exists.
func TestSwitchWarnsAboutUnknownKeysAndPreservesThem(t *testing.T) {
	claude := isolate(t)
	seedAccount(t, "u-1", "a@example.com")

	live := `{"claudeAiOauth":{"accessToken":"OTHER","refreshToken":"OTHER-RT"},"somethingNew":{"a":1}}`
	if err := os.WriteFile(ccpath.CredentialsPath(), []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, errOut, top := runRoot(t, "switch", "1")
	if code != ExitOK {
		t.Fatalf("switch = %d (%s), want 0", code, top)
	}
	if !strings.Contains(errOut, "somethingNew") {
		t.Fatalf("stderr = %q, want the unrecognized key named", errOut)
	}

	raw, err := os.ReadFile(ccpath.CredentialsPath())
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
	if err := os.WriteFile(ccpath.CredentialsPath(), []byte("{ not json"), 0o600); err != nil {
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
