package cclink

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The exec layer is exercised against a fixture `security`, because the real
// one exists on exactly one platform and this project is developed on another.
// Build-tagging the spawns to darwin would have shipped the deadline, the kill
// and the exit-code mapping completely unexercised -- which is the entire
// substance of the file, and the dangerous part of it: a locked keychain on a
// headless session does not fail, it blocks on an unlock dialog nobody will
// answer, so a spawn with no deadline never returns.
//
// The fixture is this test binary re-executed in a role, the same shape
// internal/daemon uses. A shell script would have been shorter and would have
// made the suite depend on /bin/sh being present and on how it quotes.

const (
	// securityRoleEnv turns a re-executed copy of this binary into the fake
	// `security`. sleeperRoleEnv turns it into a process that does nothing but
	// hold a file descriptor open, which is what the WaitDelay case needs.
	securityRoleEnv = "CCDAD_TEST_SECURITY"
	sleeperRoleEnv  = "CCDAD_TEST_SECURITY_SLEEPER"

	securityArgvEnv   = "CCDAD_TEST_SECURITY_ARGV"
	securityStdoutEnv = "CCDAD_TEST_SECURITY_STDOUT"
	securityStderrEnv = "CCDAD_TEST_SECURITY_STDERR"
	securityExitEnv   = "CCDAD_TEST_SECURITY_EXIT"
	securityHangEnv   = "CCDAD_TEST_SECURITY_HANG"
	securityLingerEnv = "CCDAD_TEST_SECURITY_LINGER"
)

// TestMain routes on the environment before testing ever sees the argument
// list. It has to: the roles below are started with `security`'s arguments on
// argv, which the testing package would reject as unknown flags.
func TestMain(m *testing.M) {
	// The sleeper is checked FIRST. It is started by the fake `security`, so it
	// inherits the fake's own role variable, and a check in the other order
	// would make it a second fake instead of a sleeper.
	if os.Getenv(sleeperRoleEnv) != "" {
		os.Exit(runAsSleeper())
	}
	if os.Getenv(securityRoleEnv) != "" {
		os.Exit(runAsFakeSecurity())
	}
	os.Exit(m.Run())
}

// runAsFakeSecurity is /usr/bin/security for the length of one test: it records
// what it was asked, says what the test told it to say, and exits with the code
// the test chose.
func runAsFakeSecurity() int {
	if path := os.Getenv(securityArgvEnv); path != "" {
		// Recorded rather than asserted here: the child cannot fail a test, and
		// a child that tried would report through an exit code the test is
		// already using for something else.
		_ = os.WriteFile(path, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	if d := os.Getenv(securityHangEnv); d != "" {
		wait, err := time.ParseDuration(d)
		if err != nil {
			return 99
		}
		time.Sleep(wait)
	}
	if os.Getenv(securityLingerEnv) != "" {
		// Hand this process's stdout -- the parent's pipe -- to a child that
		// outlives it. The parent's Wait then has a descriptor to block on that
		// the exit of this process does not close, which is the hazard the
		// deadline alone does not cover.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), sleeperRoleEnv+"=1")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			return 98
		}
	}
	if out := os.Getenv(securityStdoutEnv); out != "" {
		fmt.Fprint(os.Stdout, out)
	}
	if errOut := os.Getenv(securityStderrEnv); errOut != "" {
		fmt.Fprint(os.Stderr, errOut)
	}
	code, err := strconv.Atoi(os.Getenv(securityExitEnv))
	if err != nil {
		return 0
	}
	return code
}

// runAsSleeper holds its inherited stdout open and then goes away on its own.
// The bound is what keeps a failed run from leaving a process behind; nothing
// in the test releases it, because the point is that the test does not wait.
func runAsSleeper() int {
	time.Sleep(5 * time.Second)
	return 0
}

// fakeSecurity is one fixture `security`: what it should print, what it should
// exit with, and how it should misbehave.
type fakeSecurity struct {
	stdout string
	stderr string
	exit   int
	// hang is how long the process sleeps before doing anything else. Paired
	// with a shortened keychainTimeout it is the deadline case.
	hang time.Duration
	// linger makes it leak a child holding the output pipe.
	linger bool
}

// install points the exec layer at the fixture and pretends this machine is
// macOS, and returns the path the fixture will record its argv into.
//
// Every package-level knob it touches is restored, including the ones a test
// does not change: a leaked keychainGOOS of "darwin" would make every later
// test in the package try to spawn the real /usr/bin/security.
func (f fakeSecurity) install(t *testing.T) string {
	t.Helper()

	saved := struct {
		path      string
		goos      string
		timeout   time.Duration
		waitDelay time.Duration
	}{securityPath, keychainGOOS, keychainTimeout, keychainWaitDelay}
	t.Cleanup(func() {
		securityPath, keychainGOOS = saved.path, saved.goos
		keychainTimeout, keychainWaitDelay = saved.timeout, saved.waitDelay
	})

	securityPath = os.Args[0]
	keychainGOOS = "darwin"

	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv(securityRoleEnv, "1")
	t.Setenv(securityArgvEnv, argv)
	t.Setenv(securityStdoutEnv, f.stdout)
	t.Setenv(securityStderrEnv, f.stderr)
	t.Setenv(securityExitEnv, strconv.Itoa(f.exit))
	t.Setenv(securityHangEnv, "")
	if f.hang > 0 {
		t.Setenv(securityHangEnv, f.hang.String())
	}
	t.Setenv(securityLingerEnv, "")
	if f.linger {
		t.Setenv(securityLingerEnv, "1")
	}
	return argv
}

// recordedArgv is what the fixture was actually invoked with.
func recordedArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fixture recorded no argv: %v", err)
	}
	return strings.Split(string(data), "\n")
}

var testItem = KeychainItem{Service: "Claude Code-credentials", Account: "tester"}

// Nothing is spawned off macOS, and "nothing" is asserted rather than assumed:
// the fixture records its argv the moment it starts, so the absence of that
// file is proof no process ran.
func TestRunSecurityRefusesOffDarwin(t *testing.T) {
	argv := fakeSecurity{}.install(t)
	keychainGOOS = "linux"

	present, err := testItem.Present(context.Background())
	if !errors.Is(err, ErrKeychainUnsupported) {
		t.Fatalf("err = %v, want ErrKeychainUnsupported", err)
	}
	if present {
		t.Fatal("Present reported an item on a platform with no keychain")
	}
	if _, statErr := os.Stat(argv); statErr == nil {
		t.Fatal("a security process was started on a platform with no keychain")
	}
}

// The attribute-only lookup, and the assertion is the WHOLE argument vector
// against a literal. -w is what decrypts the secret and what can raise the
// "wants to use your keychain" dialog, so its absence is the property; asserting
// only that the call succeeded would pass just as well with it present.
func TestPresentLooksUpAttributesWithoutDecrypting(t *testing.T) {
	argv := fakeSecurity{exit: 0}.install(t)

	present, err := testItem.Present(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("Present = false, want true for an item that is there")
	}

	want := []string{"find-generic-password", "-a", "tester", "-s", "Claude Code-credentials"}
	if got := recordedArgv(t, argv); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

// Exit 44 is errSecItemNotFound, and it is the ONLY non-zero exit that means
// "there is no such item".
func TestPresentReportsAbsence(t *testing.T) {
	fakeSecurity{exit: 44}.install(t)

	present, err := testItem.Present(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("Present = true, want false for exit 44")
	}
}

// A locked keychain must not read as a clean machine. This is "cannot tell is
// not no" at the level of one exit code, and it is the mistake that would make
// the whole check worthless: a machine with a stale credential and a locked
// keychain would be reported as having neither.
func TestPresentTreatsALockedKeychainAsUnknown(t *testing.T) {
	fakeSecurity{
		exit:   1,
		stderr: "security: SecKeychainItemCopyContent: The specified keychain is locked.",
	}.install(t)

	present, err := testItem.Present(context.Background())
	if present {
		t.Fatal("Present = true on a failed lookup")
	}
	var kcErr *KeychainError
	if !errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want a *KeychainError", err)
	}
	if kcErr.Op != "find-generic-password" {
		t.Fatalf("Op = %q, want the subcommand that failed", kcErr.Op)
	}
	if !strings.Contains(kcErr.Detail(), "unlock") {
		t.Fatalf("Detail() = %q, want the sentence that tells the user to unlock", kcErr.Detail())
	}
}

// The read passes -w, in Claude Code's own argument order, and strips exactly
// the newline `security` adds.
func TestReadReturnsTheSecret(t *testing.T) {
	argv := fakeSecurity{stdout: "{\"claudeAiOauth\":{}}\n"}.install(t)

	secret, ok, err := testItem.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Read reported no item")
	}
	if want := "{\"claudeAiOauth\":{}}"; secret != want {
		t.Fatalf("secret = %q, want %q", secret, want)
	}

	want := []string{"find-generic-password", "-a", "tester", "-w", "-s", "Claude Code-credentials"}
	if got := recordedArgv(t, argv); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestReadReportsAbsence(t *testing.T) {
	fakeSecurity{exit: 44}.install(t)

	secret, ok, err := testItem.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok || secret != "" {
		t.Fatalf("Read = (%q, %v), want (\"\", false)", secret, ok)
	}
}

// Delete is asked for a STATE, so an item that was already gone is success --
// and the argument vector carries no -w, because deleting does not need the
// secret and asking for it would be a prompt for nothing.
func TestDeleteTreatsAbsenceAsSuccess(t *testing.T) {
	argv := fakeSecurity{exit: 44}.install(t)

	if err := testItem.Delete(context.Background()); err != nil {
		t.Fatalf("Delete on an absent item = %v, want nil", err)
	}
	want := []string{"delete-generic-password", "-a", "tester", "-s", "Claude Code-credentials"}
	if got := recordedArgv(t, argv); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestDeleteReportsARealFailure(t *testing.T) {
	fakeSecurity{
		exit:   1,
		stderr: "security: SecKeychainItemDelete: User interaction is not allowed.",
	}.install(t)

	err := testItem.Delete(context.Background())
	var kcErr *KeychainError
	if !errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want a *KeychainError", err)
	}
	if kcErr.Op != "delete-generic-password" {
		t.Fatalf("Op = %q, want the subcommand that failed", kcErr.Op)
	}
	if !strings.Contains(kcErr.Detail(), "headless") {
		t.Fatalf("Detail() = %q, want the non-interactive sentence", kcErr.Detail())
	}
}

// The deadline, against a fixture that genuinely does not finish. A fixture
// that exited on its own would prove nothing here: a `security` blocked on an
// unlock dialog nobody will answer is the entire premise, and the sleep is what
// stands in for it.
func TestSpawnIsKilledAtTheDeadline(t *testing.T) {
	fakeSecurity{hang: 30 * time.Second}.install(t)
	keychainTimeout = 150 * time.Millisecond

	start := time.Now()
	_, err := testItem.Present(context.Background())
	elapsed := time.Since(start)

	var kcErr *KeychainError
	if !errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want a *KeychainError", err)
	}
	if kcErr.failure != failTimedOut {
		t.Fatalf("failure = %q, want %q", kcErr.failure, failTimedOut)
	}
	// Generous, but far below the fixture's own 30 s: this fails if the
	// deadline is not enforced at all, and does not flake on a loaded machine.
	if elapsed > 10*time.Second {
		t.Fatalf("the call took %s; the deadline did not stop it", elapsed)
	}
}

// Killing the child is not the same as ending the wait. Here the child exits
// promptly and correctly, and a grandchild it started holds the output pipe
// open -- so a Wait with no WaitDelay blocks for as long as the grandchild
// lives, with the deadline already satisfied and nothing left to cancel.
func TestSpawnStopsWaitingOnALingeringGrandchild(t *testing.T) {
	fakeSecurity{linger: true, stdout: "irrelevant\n"}.install(t)
	keychainTimeout = 30 * time.Second
	keychainWaitDelay = 200 * time.Millisecond

	start := time.Now()
	_, err := testItem.Present(context.Background())
	elapsed := time.Since(start)

	var kcErr *KeychainError
	if !errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want a *KeychainError", err)
	}
	if kcErr.failure != failLingering {
		t.Fatalf("failure = %q, want %q", kcErr.failure, failLingering)
	}
	// The grandchild sleeps 5 s. Anything under that proves the wait was
	// abandoned rather than served.
	if elapsed > 4*time.Second {
		t.Fatalf("the call took %s; it waited for the grandchild", elapsed)
	}
}

// A missing binary is its own diagnosis. Without this branch it would classify
// as "other" and the report would say nothing the user could act on.
//
// Both spellings, because there are two and only one of them happens here.
// An absolute path that is not there fails with fs.ErrNotExist; a name with no
// extension fails Go's Windows PATHEXT lookup with exec.ErrNotFound instead,
// which is what the Windows CI leg would produce and what this machine can only
// reach through a bare name.
func TestMissingSecurityBinary(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"an absolute path that is not there", filepath.Join(t.TempDir(), "no-such-security")},
		{"a name no lookup resolves", "ccdad-no-such-security-binary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeSecurity{}.install(t)
			securityPath = tc.path

			_, err := testItem.Present(context.Background())
			var kcErr *KeychainError
			if !errors.As(err, &kcErr) {
				t.Fatalf("err = %v, want a *KeychainError", err)
			}
			if kcErr.failure != failSecurityMissing {
				t.Fatalf("failure = %q, want %q", kcErr.failure, failSecurityMissing)
			}
		})
	}
}

// A caller who cancels gets their own error back, not a keychain diagnosis:
// a daemon shutting down did not discover anything about this machine's
// keychain, and reporting that it did would put a false finding in a report.
func TestCallerCancellationIsNotAKeychainFailure(t *testing.T) {
	fakeSecurity{hang: 30 * time.Second}.install(t)
	keychainTimeout = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := testItem.Present(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	var kcErr *KeychainError
	if errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want no keychain diagnosis", err)
	}
}

func TestLoadKeychainCredentialsDecodes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	t.Setenv("USER", "tester")
	fakeSecurity{stdout: "{\"claudeAiOauth\":{\"accessToken\":\"a\"},\"mcpOAuth\":{}}\n"}.install(t)

	blob, ok, err := LoadKeychainCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("LoadKeychainCredentials reported no item")
	}
	if _, has := blob["claudeAiOauth"]; !has {
		t.Fatalf("blob = %v, want the login key", blob)
	}
	if _, has := blob["mcpOAuth"]; !has {
		t.Fatalf("blob = %v, want the machine-scoped key preserved", blob)
	}
}

func TestLoadKeychainCredentialsReportsAnAbsentItem(t *testing.T) {
	t.Setenv("USER", "tester")
	fakeSecurity{exit: 44}.install(t)

	blob, ok, err := LoadKeychainCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok || blob != nil {
		t.Fatalf("LoadKeychainCredentials = (%v, %v), want (nil, false)", blob, ok)
	}
}

// `security -w` hex-encodes anything it cannot print, so output that is not
// JSON means the item is not holding what its name says. Reporting that as "no
// login" would hide a machine whose keychain has something else entirely in it.
func TestLoadKeychainCredentialsRefusesNonJSON(t *testing.T) {
	t.Setenv("USER", "tester")
	fakeSecurity{stdout: "7b2261223a317d\n"}.install(t)

	_, ok, err := LoadKeychainCredentials(context.Background())
	if err == nil {
		t.Fatal("err = nil, want a parse failure")
	}
	if ok {
		t.Fatal("ok = true for output that is not a credential")
	}
}

// The item a probe reports on is the one this environment derives, not a
// hardcoded name -- otherwise a user with CLAUDE_CONFIG_DIR set would be told
// about an item Claude Code never wrote.
func TestProbeCredentialKeychainItemUsesTheDerivedItem(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/cc")
	unsetEnv(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "")
	t.Setenv("USER", "tester")
	argv := fakeSecurity{exit: 0}.install(t)

	found, err := ProbeCredentialKeychainItem(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !found.Present {
		t.Fatal("Present = false, want true")
	}
	if want := "Claude Code-credentials-aa3d8c96"; found.Item.Service != want {
		t.Fatalf("Item.Service = %q, want %q", found.Item.Service, want)
	}
	// One candidate, so one spawn. A composed value must not cost the second
	// lookup the decomposed case needs.
	if len(found.Checked) != 1 {
		t.Fatalf("Checked = %v, want exactly one candidate", found.Checked)
	}
	want := []string{"find-generic-password", "-a", "tester", "-s", "Claude Code-credentials-aa3d8c96"}
	if got := recordedArgv(t, argv); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

// The defect the era measurement turned up: 1.0.30 through 2.1.37 hashed
// CLAUDE_CONFIG_DIR as it came, and only 2.1.38 started normalizing to NFC. A
// decomposed value therefore has an item under EACH digest depending on which
// build wrote it, and a probe that tried only the composed one answered "no
// legacy item" with certainty about a name it had never looked for.
//
// The fixture answers 44 (absent) for every lookup, so the assertion is on what
// was ASKED. `security` is invoked twice and the second invocation carries the
// raw digest, which is the one a machine on 1.0.30-2.1.37 wrote.
func TestProbeCredentialKeychainItemTriesBothSpellingsOfADecomposedPath(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", nfdCafe)
	unsetEnv(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "")
	t.Setenv("USER", "tester")
	argv := fakeSecurity{exit: securityNotFoundCode}.install(t)

	found, err := ProbeCredentialKeychainItem(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if found.Present {
		t.Fatal("Present = true for a fixture that reports absence")
	}
	var got []string
	for _, item := range found.Checked {
		got = append(got, item.Service)
	}
	want := []string{"Claude Code-credentials-0873cca0", "Claude Code-credentials-16eb4464"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Checked = %q, want %q", got, want)
	}
	// And the second name really reached `security` rather than only appearing
	// in the report. The fixture truncates its argv file on every run, so what
	// is left is the LAST invocation -- which must be the raw-digest lookup.
	wantArgv := []string{"find-generic-password", "-a", "tester", "-s", "Claude Code-credentials-16eb4464"}
	if got := recordedArgv(t, argv); !reflect.DeepEqual(got, wantArgv) {
		t.Fatalf("the last spawn was %q, want %q", got, wantArgv)
	}
}

// A probe that could not answer must stop, not fall through to the next
// candidate. "The keychain is locked" is not "the item is not under this name",
// and a loop that kept going would turn one unanswerable lookup into a
// confident "no legacy item" -- the exact failure mode the multi-candidate
// probe was added to remove, reintroduced by the fix for it.
func TestProbeCredentialKeychainItemStopsAtAnUnanswerableLookup(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", nfdCafe)
	unsetEnv(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "")
	t.Setenv("USER", "tester")
	argv := fakeSecurity{exit: 1, stderr: "The user name or passphrase you entered is not correct."}.install(t)

	found, err := ProbeCredentialKeychainItem(context.Background())
	if err == nil {
		t.Fatal("err = nil for a lookup that could not answer")
	}
	if found.Present {
		t.Fatal("Present = true alongside an error")
	}
	// The FIRST name is what ran, and nothing after it. If the loop had
	// continued, the fixture's last argv would name the raw digest.
	want := []string{"find-generic-password", "-a", "tester", "-s", "Claude Code-credentials-0873cca0"}
	if got := recordedArgv(t, argv); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q — the probe continued past an error", got, want)
	}
}

// Write installs the secret under the item, in Claude Code's own argument
// shape: -U so an item that already exists is updated rather than refused, and
// the payload as hex under -X because that is what its own writer passes.
//
// The hex is asserted rather than the plaintext: -X is what makes `security`
// treat the argument as data rather than as a password to be interpreted, and
// a Write that passed the JSON raw would store something else.
func TestWriteInstallsTheSecretAsHex(t *testing.T) {
	argv := fakeSecurity{}.install(t)

	const secret = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-x"}}`
	if err := testItem.Write(context.Background(), secret); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"add-generic-password", "-U",
		"-a", "tester", "-s", "Claude Code-credentials",
		"-X", hex.EncodeToString([]byte(secret)),
	}
	if got := recordedArgv(t, argv); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

// A `security` that exits non-zero and prints nothing leaves its EXIT CODE as
// the only fact about the failure, so the error has to carry it.
//
// This is not a hypothetical. A daemon ticking once a second wrote 11,300
// copies of `security find-generic-password: empty` over three hours, and that
// sentence names no exit code, no store and no remedy -- so the failure could
// not be told apart from any other silent one, and the loop that produced it
// could not be diagnosed from its own log.
//
// The old spelling was worse than uninformative. "empty" reads as a verdict on
// the ITEM, which is the one thing it never means: an item whose blob is
// genuinely zero-length exits 0, and TestReadTreatsAZeroLengthBlobAsAValue
// pins that. It classifies an empty STDERR.
func TestReadNamesTheExitCodeWhenSecuritySaysNothing(t *testing.T) {
	fakeSecurity{exit: 60}.install(t)

	_, _, err := testItem.Read(context.Background())
	var kcErr *KeychainError
	if !errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want a *KeychainError", err)
	}
	if !strings.Contains(err.Error(), "60") {
		t.Fatalf("Error() = %q, want the exit code in it", err.Error())
	}
	if strings.Contains(err.Error(), "empty") {
		t.Fatalf("Error() = %q, still says \"empty\", which names the item rather than the spawn", err.Error())
	}
	if !strings.Contains(kcErr.Detail(), "60") {
		t.Fatalf("Detail() = %q, want the exit code -- it is the only evidence there is", kcErr.Detail())
	}
}

// The blob a silent failure gets confused with: zero length is a VALUE, it
// exits 0, and it is not an error. Measured against the real /usr/bin/security
// on macOS 15, which returns code 0 and an empty stdout for such an item.
func TestReadTreatsAZeroLengthBlobAsAValue(t *testing.T) {
	fakeSecurity{stdout: ""}.install(t)

	secret, ok, err := testItem.Read(context.Background())
	if err != nil {
		t.Fatalf("Read on a zero-length blob = %v, want no error", err)
	}
	if !ok {
		t.Fatal("Read reported no item for an item that is there and holds nothing")
	}
	if secret != "" {
		t.Fatalf("secret = %q, want the empty value", secret)
	}
}

// A silent `security` is not a `security` that said nothing USEFUL: its exit
// code is the OSStatus, and for the keychain band that is a name.
//
// 36 is the one this arrived as. A daemon logged `said-nothing (exit 36)` on
// the first tick after a restart and every tick for the next two minutes, and
// 36 is errSecInteractionNotAllowed -- the keychain refusing a non-interactive
// lookup, which is a thing ccdad already has the right sentence for and could
// not reach, because it reached it only from stderr text that this path does
// not produce.
func TestASilentExitIsClassifiedByItsCode(t *testing.T) {
	fakeSecurity{exit: 36}.install(t)

	_, _, err := testItem.Read(context.Background())
	var kcErr *KeychainError
	if !errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want a *KeychainError", err)
	}
	if strings.Contains(err.Error(), "said-nothing") {
		t.Fatalf("Error() = %q, want the code's own name rather than the fallback", err.Error())
	}
	if !strings.Contains(err.Error(), "36") {
		t.Fatalf("Error() = %q, want the exit code still in it", err.Error())
	}
	// The sentence that tells the user what to do about it, which is the whole
	// point of naming the code.
	if !strings.Contains(kcErr.Detail(), "non-interactive") {
		t.Fatalf("Detail() = %q, want the sentence for a refused non-interactive lookup", kcErr.Detail())
	}
}

// Every code in the keychain band that ccdad can name, and the ones it must
// NOT. The low byte is ambiguous across the whole OSStatus space -- only this
// band is being read -- so a code outside what is listed here keeps the
// honestly empty answer rather than being guessed at.
func TestSilentExitCodesAreNamedOnlyWhereTheyAreKnown(t *testing.T) {
	for _, tc := range []struct {
		code int
		want keychainFailure
	}{
		{36, failNoInteraction}, // errSecInteractionNotAllowed
		{37, failNoKeychain},    // errSecNoDefaultKeychain
		{45, failDuplicateItem}, // errSecDuplicateItem
		{50, failNoKeychain},    // errSecNoSuchKeychain
		{51, failAuthFailed},    // errSecAuthFailed
		{128, failUserCanceled}, // errSecUserCanceled
		{29, failSaidNothing},   // errSecInteractionRequired: a real code, but
		{53, failSaidNothing},   // errSecNotAvailable: no sentence here says
		{60, failSaidNothing},   // it, and a wrong one is worse than a number.
		{1, failSaidNothing},
		{0, failSaidNothing},
	} {
		if got := classifyKeychainFailure("", tc.code); got != tc.want {
			t.Errorf("exit %d classified as %q, want %q", tc.code, got, tc.want)
		}
	}
}

// stderr OUTRANKS the code, and the order is load-bearing for the reason
// classifyKeychainError's own header gives: it mirrors Claude Code's classifier
// so the two agree about why one spawn failed. The code is a fallback for the
// path that produces no stderr, never a second opinion about one that did.
func TestStderrStillDecidesWhenThereIsAny(t *testing.T) {
	got := classifyKeychainFailure("security: SecKeychainSearchCopyNext: User canceled the operation.", 36)
	if got != failUserCanceled {
		t.Fatalf("classified as %q, want the stderr's verdict rather than the code's", got)
	}
}

// Present is the path doctor's probe takes, and it is where the sentence for a
// refused lookup actually reaches a person -- the keychain row prints Detail().
// Read having the classification is not evidence that this does: they build
// their errors at separate call sites, and a mutation that reverted only this
// one went unnoticed by every test above.
func TestPresentAlsoClassifiesASilentExitByItsCode(t *testing.T) {
	fakeSecurity{exit: 36}.install(t)

	present, err := testItem.Present(context.Background())
	if present {
		t.Fatal("Present = true on a failed lookup")
	}
	var kcErr *KeychainError
	if !errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want a *KeychainError", err)
	}
	if !strings.Contains(kcErr.Detail(), "non-interactive") {
		t.Fatalf("Detail() = %q, want the sentence doctor prints for a refused lookup", kcErr.Detail())
	}
}

// Write is the other live path: the install a switch performs. A switch that
// fails because the keychain refused the session has to say THAT, or the report
// is "the login did not change" with no reason attached.
func TestWriteAlsoClassifiesASilentExitByItsCode(t *testing.T) {
	fakeSecurity{exit: 36}.install(t)

	err := testItem.Write(context.Background(), "{}")
	var kcErr *KeychainError
	if !errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want a *KeychainError", err)
	}
	if kcErr.Op != "add-generic-password" {
		t.Fatalf("Op = %q, want the subcommand that failed", kcErr.Op)
	}
	if !strings.Contains(kcErr.Detail(), "non-interactive") {
		t.Fatalf("Detail() = %q, want the refused-lookup sentence on the write path too", kcErr.Detail())
	}
}

// Delete is the one nothing in ccdad calls, and its header carries the ruling
// that says so. It builds its error at a call site of its own all the same, and
// a classifier that is right at three of four sites is a trap for whoever
// lifts the ruling: the surviving mutant is closed here rather than left as an
// exercise.
func TestDeleteAlsoClassifiesASilentExitByItsCode(t *testing.T) {
	fakeSecurity{exit: 36}.install(t)

	err := testItem.Delete(context.Background())
	var kcErr *KeychainError
	if !errors.As(err, &kcErr) {
		t.Fatalf("err = %v, want a *KeychainError", err)
	}
	if !strings.Contains(kcErr.Detail(), "non-interactive") {
		t.Fatalf("Detail() = %q, want the refused-lookup sentence on the delete path too", kcErr.Detail())
	}
}
