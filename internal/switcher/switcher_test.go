package switcher

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

func TestExecuteInstallsTheTargetAndRecordsIt(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")

	s := openStore(t)
	res, err := Execute(s, Request{Target: target, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
	if !bytes.Contains(readLive(t), []byte("RT-u-2")) {
		t.Fatalf("the credentials file does not hold the target's login: %s", readLive(t))
	}
	if got := openStore(t).ActiveUUID(); got != "u-2" {
		t.Fatalf("ActiveUUID = %q, want u-2", got)
	}
	if at, to := lastSwitch(t); to != "u-2" || at.IsZero() {
		t.Fatalf("cooldown stamp = (%v, %q), want a stamp naming u-2", at, to)
	}
}

// The already-on answer is a no-op, and a no-op must not touch the file. Claude
// Code watches the credentials file for change; a rewrite with identical content
// is still a change to everything that looks at mtime.
func TestExecuteReportsAlreadyOnWithoutWriting(t *testing.T) {
	isolate(t)
	target := seed(t, "u-1", "one@example.com")
	liveAs(t, "u-1")
	before := readLive(t)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != AlreadyOn {
		t.Fatalf("outcome = %v, want %v", res.Outcome, AlreadyOn)
	}
	if !bytes.Equal(before, readLive(t)) {
		t.Fatal("a no-op rewrote the credentials file")
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("a no-op stamped the anti-flap cooldown")
	}
}

// The already-on question is asked INSIDE the credential locks, against the file
// as it is at the moment of the write. Only a read taken there can see a login
// that landed while this call was blocked on the lock — which is exactly the
// user running `ccdad switch` by hand while the daemon was mid-swap.
func TestExecuteDecidesAlreadyOnFromTheFileUnderTheLock(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")

	held, err := cclock.AcquireCredentials(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
		done <- outcome{res, err}
	}()

	select {
	case got := <-done:
		t.Fatalf("Execute returned (%v, %v) while the credential locks were held; it never took them", got.res.Outcome, got.err)
	case <-time.After(300 * time.Millisecond):
	}

	// The account the engine was moving TO becomes live by another route.
	liveAs(t, "u-2")
	after := readLive(t)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.res.Outcome != AlreadyOn {
			t.Fatalf("outcome = %v, want %v — the already-on question was asked before the lock", got.res.Outcome, AlreadyOn)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Execute did not return after the locks were released")
	}
	if !bytes.Equal(after, readLive(t)) {
		t.Fatal("Execute rewrote a file that was already what it wanted")
	}
}

// An unattended swap is conditional on the login it was decided against. A user
// who ran `ccdad switch` by hand between the engine's decision and this write
// made a choice seconds ago, and the daemon overwriting it is the flap the whole
// anti-flap layer exists to prevent — it cannot be caught by the cooldown, which
// the daemon had already read.
func TestAnUnattendedExecuteStandsDownWhenTheLiveLoginChanged(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	seed(t, "u-3", "three@example.com")
	liveAs(t, "u-3")
	before := readLive(t)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Raced {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Raced)
	}
	if !bytes.Equal(before, readLive(t)) {
		t.Fatal("a stood-down swap still rewrote the credentials file")
	}
	if got := openStore(t).ActiveUUID(); got == "u-2" {
		t.Fatal("a stood-down swap recorded the target as active")
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("a stood-down swap stamped the anti-flap cooldown")
	}
}

// The attended path does not stand down. A human typed the command and is
// watching the result, so a changed login is something to report, not a reason
// to refuse the thing they just asked for.
func TestAnAttendedExecuteProceedsThroughAChangedLiveLogin(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	seed(t, "u-3", "three@example.com")
	liveAs(t, "u-3")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
	if !bytes.Contains(readLive(t), []byte("RT-u-2")) {
		t.Fatalf("the credentials file does not hold the target's login: %s", readLive(t))
	}
}

// A lock stolen mid-hold means Claude Code may have written the file after we
// did. Recording the target as active, or stamping a cooldown, would assert a
// swap that may not be on disk — and the cooldown would then hold the engine off
// its own retry.
func TestExecuteRecordsNothingWhenTheWriteWasNotDurable(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")

	restore := activateWith
	t.Cleanup(func() { activateWith = restore })
	activateWith = func(decide func(cclink.Blob) (cclink.Blob, error)) error {
		if _, err := decide(cclink.Blob{"claudeAiOauth": json.RawMessage(`{"refreshToken":"RT-u-1"}`)}); err != nil {
			return err
		}
		return cclock.ErrCompromised
	}

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if !errors.Is(err, cclock.ErrCompromised) {
		t.Fatalf("Execute = %v, want an error satisfying errors.Is(err, cclock.ErrCompromised)", err)
	}
	if res.Outcome != NotSwitched {
		t.Fatalf("outcome = %v, want %v on an error", res.Outcome, NotSwitched)
	}
	if got := openStore(t).ActiveUUID(); got == "u-2" {
		t.Fatal("a compromised write was recorded as the active account")
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("a compromised write stamped the anti-flap cooldown")
	}
}

// Claude Code reads CLAUDE_CODE_OAUTH_TOKEN in preference to the credentials
// file, so with it set the swap changes nothing about what the session uses. A
// daemon evaluating every few minutes would switch into the void forever; it has
// to refuse, and say why, rather than warn into a log nobody reads.
func TestAnUnattendedExecuteRefusesWhenTheEnvironmentTokenWins(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-SOMETHING")
	before := readLive(t)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Overridden {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Overridden)
	}
	if !bytes.Equal(before, readLive(t)) {
		t.Fatal("a refused swap still rewrote the credentials file")
	}
	if at, _ := lastSwitch(t); !at.IsZero() {
		t.Fatal("a refused swap stamped the anti-flap cooldown")
	}
}

// Attended, the same environment is a note rather than a refusal: the user asked
// for the swap, the swap is what they get, and the reason it appears to do
// nothing is reported alongside it.
func TestAnAttendedExecuteSwitchesAnywayAndReportsTheEnvironmentToken(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-SOMETHING")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
	if !res.EnvTokenWins {
		t.Fatal("Result.EnvTokenWins is false; the caller has nothing to warn about")
	}
}

// Claude Code reads a setup token from the environment only, so there is no file
// to install it into. The executor refuses before taking a lock rather than
// handing cclink a snapshot it will reject for a reason no caller can act on.
func TestExecuteRefusesAnAccountWithNoLoginToInstall(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seedToken(t, "u-tok", "tok@example.com", "setup-token", "sk-ant-oat01-X")
	liveAs(t, "u-1")
	before := readLive(t)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if !errors.Is(err, ErrNoLogin) {
		t.Fatalf("Execute = %v, want an error satisfying errors.Is(err, ErrNoLogin)", err)
	}
	// The zero Outcome must not read as a success. A caller that logs the
	// Result alongside the error would otherwise report every failure as a
	// completed switch.
	if res.Outcome != NotSwitched {
		t.Fatalf("outcome = %v, want %v on an error", res.Outcome, NotSwitched)
	}
	if !bytes.Equal(before, readLive(t)) {
		t.Fatal("a refused target still rewrote the credentials file")
	}
}

// Drift in the credentials file is demonstrated, not hypothetical. The keys
// are reported from the file read UNDER the lock, so the report describes the
// file that was actually merged.
func TestExecuteReportsTheUnknownKeysItPreserved(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	writeLive(t, `{"claudeAiOauth":{"accessToken":"AT-RT-u-1","refreshToken":"RT-u-1"},"somethingNew":{"a":1}}`)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.UnknownKeys) != 1 || res.UnknownKeys[0] != "somethingNew" {
		t.Fatalf("UnknownKeys = %v, want [somethingNew]", res.UnknownKeys)
	}
	if !bytes.Contains(readLive(t), []byte("somethingNew")) {
		t.Fatal("the unrecognized key was not preserved")
	}
}

// The login now in place outranks any stored API key, so nothing is broken by
// leaving one — but it becomes the credential again the moment the login goes
// away, silently and as a different account. ccdad clears its own; a key ccdad
// did not install stays.
func TestExecuteClearsCcdadsOwnStoredAPIKey(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	owner := seedToken(t, "u-key", "key@example.com", cclink.APIKeyKind, "sk-ant-api-MINE")
	liveAs(t, "u-1")
	if err := cclink.UpdateGlobalConfig(func(g *cclink.GlobalConfig) error {
		return cclink.SetPrimaryAPIKey(g, "sk-ant-api-MINE")
	}); err != nil {
		t.Fatal(err)
	}

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.ClearedKey || res.ClearedKeyOwner.UUID != owner.UUID {
		t.Fatalf("ClearedKey = %v, owner = %q, want the key cleared for %s", res.ClearedKey, res.ClearedKeyOwner.UUID, owner.UUID)
	}
	cfg, err := cclink.LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cclink.PrimaryAPIKey(cfg); ok {
		t.Fatal("the stored API key survived the switch")
	}
}

// Force is the explicit bypass of the already-on answer, and only of that.
func TestForceInstallsTheAccountThatIsAlreadyLive(t *testing.T) {
	isolate(t)
	target := seed(t, "u-1", "one@example.com")
	writeLive(t, `{"claudeAiOauth":{"accessToken":"stale","refreshToken":"RT-u-1"}}`)

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
	if bytes.Contains(readLive(t), []byte("stale")) {
		t.Fatalf("--force did not reinstall the stored snapshot: %s", readLive(t))
	}
}

// A cooldown that cannot be stamped is reported, never returned as a failure:
// the credentials file has already been written by the time it is attempted, so
// failing here would report a completed switch as an error.
func TestACooldownThatCannotBeStampedDoesNotFailTheSwitch(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")

	// A directory where the state file belongs: the write fails, the swap does
	// not.
	statePath := mustPath(strategy.StatePath())
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1"})
	if err != nil {
		t.Fatalf("Execute = %v, want the switch to stand", err)
	}
	if res.Outcome != Switched {
		t.Fatalf("outcome = %v, want %v", res.Outcome, Switched)
	}
	if res.CooldownErr == nil {
		t.Fatal("Result.CooldownErr is nil; the caller cannot report what it lost")
	}
	if !strings.Contains(res.CooldownErr.Error(), "cooldown") {
		t.Fatalf("CooldownErr = %v, want it to name what was lost", res.CooldownErr)
	}
	if !bytes.Contains(readLive(t), []byte("RT-u-2")) {
		t.Fatal("the switch itself did not land")
	}
}
