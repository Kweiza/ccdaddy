package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// foreignClaim describes a machine where a DIFFERENT ccdad store's engine holds
// this credential home.
//
// It stands in for the KERNEL LOCK only. The owner document is real, written to
// the real path and read back by the real reader, so everything these tests
// measure — the exit code, the words, whether a spawn happened — runs through
// production code. That a claim actually excludes a second process is proved in
// internal/credhome against a real second process, because a fake cannot
// establish a kernel fact.
func foreignClaim(t *testing.T, store string) {
	t.Helper()
	dir := filepath.Join(mustPath(credhome.Home()), credhome.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schemaVersion":1,"store":"` + store + `","pid":4242}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, credhome.OwnerFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := credhome.SetTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, nil
	})
	t.Cleanup(restore)
}

// `daemon start` has to answer this itself. The child is detached and its exit
// status is released, so a refusal inside daemon.Run reaches daemon.log and
// nothing else — and worse, the child holds the SINGLETON for its whole claim
// retry while being refused, which is long enough for the start's own wait to
// see it and report success for a process that is already dead.
//
// The exit code alone would not be enough to catch that, which is why the spawn
// count and the message are asserted too.
func TestDaemonStartRefusesACredentialHomeAnotherStoreDrives(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	foreignClaim(t, "/other/ccdad")

	code, _, stderr, _ := runRoot(t, "daemon", "start")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d — 3 is the code operators are told to ignore, and the documented "+
			"'status; [ $? -eq 5 ] && start' loop would spin on it forever", code, ExitBlocked)
	}
	if f.spawns != 0 {
		t.Errorf("daemon start spawned %d children into a credential home another store drives", f.spawns)
	}
	if !strings.Contains(stderr, "/other/ccdad") {
		t.Errorf("stderr does not name the store that holds the claim:\n%s", stderr)
	}
}

// A holder that will not name itself still holds the lock, so the child's own
// Acquire refuses on it — and a parent that spawned anyway would print
// "Started the ccdad daemon." at exit 0 over a process that is already dead.
//
// The child takes the SINGLETON before the claim and keeps it through the claim
// retry, which is longer than the start's 50 ms poll interval, so the false
// success is the likely outcome rather than the rare one. That is why this
// asserts the spawn count and not only the code.
func TestDaemonStartRefusesAClaimItCannotAttribute(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	// Held, with a document that is committed and does not parse — the state
	// readOwner reports as "held by somebody I cannot name".
	dir := filepath.Join(mustPath(credhome.Home()), credhome.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, credhome.OwnerFileName), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := credhome.SetTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, nil
	})
	t.Cleanup(restore)

	code, _, stderr, _ := runRoot(t, "daemon", "start")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d — the child's Acquire refuses a nameless holder too, and a "+
			"parent that starts anyway reports success for a process that dies at once", code, ExitBlocked)
	}
	if f.spawns != 0 {
		t.Errorf("daemon start spawned %d children that credhome.Acquire will refuse", f.spawns)
	}
	if !strings.Contains(stderr, "Not starting") {
		t.Errorf("stderr does not say it refused:\n%s", stderr)
	}
}

// A probe that could not answer must not stop the daemon. Refusing there takes
// ccdad off every machine with a network home, and the daemon degrades in that
// case anyway — so the start would be refusing on behalf of a child that would
// have run.
func TestDaemonStartProceedsWhenTheClaimCannotBeProbed(t *testing.T) {
	isolate(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	restore := credhome.SetTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, os.ErrPermission
	})
	t.Cleanup(restore)

	code, _, stderr, _ := runRoot(t, "daemon", "start")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 — an unanswerable probe is not a refusal", code)
	}
	if f.spawns != 1 {
		t.Errorf("spawns = %d, want 1", f.spawns)
	}
	if !strings.Contains(stderr, "could not tell") {
		t.Errorf("the start said nothing about the probe it could not answer:\n%s", stderr)
	}
}

// A daemon refused by the claim gives the SINGLETON back on its way out, so
// rule 2's probe stays negative forever. Without this gate every allow-listed
// command forks a child that dies immediately, on every invocation, silently —
// the unbounded spawn rule 2 exists to prevent, reached by a different door.
func TestAutoStartDoesNotSpawnIntoAClaimedCredentialHome(t *testing.T) {
	isolate(t)
	enableAutoStart(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	foreignClaim(t, "/other/ccdad")

	if _, _, _, _ = runRoot(t, "list"); f.spawns != 0 {
		t.Fatalf("auto-start spawned %d daemons into a credential home another store drives", f.spawns)
	}
}

// The same hook must still fire on an ordinary machine. Without this the test
// above passes against a hook that never spawns at all, which is the cheapest
// wrong implementation available.
func TestAutoStartStillSpawnsWhenTheClaimIsFree(t *testing.T) {
	isolate(t)
	enableAutoStart(t)
	f := stubDaemonWorld(t, &fakeDaemon{})

	if _, _, _, _ = runRoot(t, "list"); f.spawns != 1 {
		t.Fatalf("spawns = %d, want 1 — auto-start stopped working entirely", f.spawns)
	}
}

// doctor names the state, and it is the only command that can: nothing else on
// the machine reports two stores driving one login.
func TestDoctorReportsAForeignCredentialHomeHolder(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	foreignClaim(t, "/other/ccdad")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "credential-home"); got != string(levelWarn) {
		t.Errorf("level = %q, want warn", got)
	}
	if d := r.detail(t, "credential-home"); !strings.Contains(d, "/other/ccdad") {
		t.Errorf("detail does not name the holding store: %s", d)
	}
}

func TestDoctorReportsAFreeCredentialHome(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "credential-home"); got != string(levelOK) {
		t.Errorf("level = %q, want ok on a machine with no engine running: %s", got, r.detail(t, "credential-home"))
	}
}

// The drift a CLAUDE_CONFIG_DIR the USER set produces: the daemon is driving
// the directory it was born in while this shell resolves another one, so its
// switches change a login nothing here reads. Every file on the machine looks
// normal; the daemon's own published document is the only place the two can be
// compared.
//
// `ccdad run --full-profile` USED to reach this state the same way and no
// longer does — auto-start's rule 3 gained a containment test at 3d9d2d6 and
// scopedSessionRefusals covers the daemon verbs a human types. An ordinary
// override is deliberately still allowed to reach it, which is what keeps this
// check load-bearing rather than dead.
func TestDoctorCatchesADaemonDrivingADifferentCredentialHome(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status:    daemon.Status{SchemaVersion: 1, PID: 4242, CredentialHome: "/somewhere/else"},
	}, nil)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "credential-home"); got != string(levelWarn) {
		t.Fatalf("level = %q, want warn: %s", got, r.detail(t, "credential-home"))
	}
	if d := r.detail(t, "credential-home"); !strings.Contains(d, "/somewhere/else") {
		t.Errorf("detail does not name the home the daemon is actually driving: %s", d)
	}
	// The sentence must not send the user hunting for a cause they cannot have
	// hit on a current build. This is the whole of the defect the queue item
	// describes: the remedy after the semicolon stays correct while the
	// diagnosis in front of it names something the tree now prevents, and
	// nothing else in this package pins the prose.
	if d := r.detail(t, "credential-home"); strings.Contains(d, "--full-profile") {
		t.Errorf("the drift sentence still blames a spawn autostart refuses since 3d9d2d6: %s", d)
	}
}

// Two spellings of ONE directory are not drift. ccdad manufactures the pair
// itself — daemon.ChildEnv pins an absolute, symlink-resolved CLAUDE_CONFIG_DIR
// into every daemon it spawns, while ccpath hands this shell's spelling back
// untouched — so a string compare warns forever on a machine where nothing is
// wrong, telling the user to restart a daemon that is driving exactly the right
// directory.
//
// A trailing separator is enough to reproduce it, and needs neither a symlink
// nor macOS: filepath.Abs cleans it for the child and nothing cleans it here.
func TestDoctorDoesNotCallTwoSpellingsOfOneHomeADrift(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	claude := mustPath(credhome.Home())
	// What this shell resolves, spelled with a trailing separator...
	t.Setenv("CLAUDE_CONFIG_DIR", claude+string(filepath.Separator))
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", claude+string(filepath.Separator))
	// ...against what a spawned daemon records, which filepath.Abs has cleaned.
	stubDaemon(t, daemon.Report{
		State:     daemon.DaemonRunning,
		HasStatus: true,
		Status:    daemon.Status{SchemaVersion: 1, PID: 4242, CredentialHome: claude},
	}, nil)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "credential-home"); got == string(levelWarn) {
		t.Fatalf("doctor called one directory two: %s", r.detail(t, "credential-home"))
	}
}

// The check must not manufacture what it reports on — doctor's first rule, and
// this one probes a lock. A lock file that exists is evidence an engine ran
// here, and creating it destroys that evidence permanently.
func TestDoctorCreatesNoCredentialHomeClaim(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)

	runDoctor(t)
	dir := filepath.Join(mustPath(credhome.Home()), credhome.DirName)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("doctor created %s inside Claude Code's credential home", dir)
	}
}

// uninstall LEAVES both files. flock is per-inode: unlinking the lock while
// another store's engine holds it leaves that engine locking an orphaned inode
// while the next one takes a fresh file — two engines, one login, which is the
// state the file exists to prevent. This command stops only THIS store's daemon
// and has no authority over any other's.
func TestUninstallLeavesTheCredentialHomeClaimInPlace(t *testing.T) {
	isolate(t)
	seedHealthyMachine(t)
	stubDaemonWorld(t, &fakeDaemon{})
	// Without this, uninstall removes os.Executable() — which under `go test`
	// is the test binary itself. The suite then fails somewhere else entirely,
	// in whichever later test re-execs it, with "no such file or directory" for
	// a path that was there a moment ago.
	fakeBinary(t)
	foreignClaim(t, "/other/ccdad")
	dir := filepath.Join(mustPath(credhome.Home()), credhome.DirName)
	// A lock file for it to leave alone. foreignClaim writes only the owner
	// document, and the residue probe stats the LOCK.
	if err := os.WriteFile(filepath.Join(dir, credhome.LockFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr, _ := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, stderr)
	}
	for _, name := range []string{credhome.LockFileName, credhome.OwnerFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("uninstall removed %s: %v", name, err)
		}
	}
	// And it says so. A user who is told "Removed <store>" and nothing else
	// has no way to learn that two files of ccdad's are still on the machine.
	if !strings.Contains(stderr, dir) {
		t.Errorf("uninstall never mentioned %s:\n%s", dir, stderr)
	}
	// The foreign holder is named, so the reader knows why they were left.
	if !strings.Contains(stderr, "/other/ccdad") {
		t.Errorf("uninstall did not say which store still holds the claim:\n%s", stderr)
	}
}

// The cron surface. `ccdad auto --once` takes no lock of its own — that is what
// makes it usable from cron alongside a daemon — so the claim is the only thing
// that stops it writing into a login another store's engine is driving.
//
// The exit code alone is not enough to assert, and that is the point of this
// test's second half. 4 is shared with "no viable target" and "no readings yet",
// so a wrong implementation that fell through to the switched branch would emit
// {"kind":"switched"} on stdout AND still exit 4 on a different pass. The NDJSON
// event is the half that lies, so it is the half asserted on.
func TestAutoOnceStandsDownWhenAnotherStoreDrivesTheLogin(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)
	foreignClaim(t, "/other/ccdad")
	before := liveUUIDOf(t)

	code, out, errOut, _ := runRoot(t, "auto", "--once", "--json")
	if code != ExitBlocked {
		t.Fatalf("exit = %d (%s), want %d", code, errOut, ExitBlocked)
	}
	if strings.Contains(out, `"kind":"switched"`) {
		t.Errorf("a stand-down told the stream it had switched:\n%s", out)
	}
	if !strings.Contains(out, `"reason":"contended"`) {
		t.Errorf("the stream does not carry the reason it stood down:\n%s", out)
	}
	if got := liveUUIDOf(t); got != before {
		t.Errorf("the live login moved from %q to %q during a stand-down", before, got)
	}
	if !strings.Contains(errOut, "/other/ccdad") {
		t.Errorf("the human output does not name the store that holds the login:\n%s", errOut)
	}
}

// The same command on an ordinary machine still switches. Without this the test
// above passes against an implementation that stands down unconditionally,
// which is the cheapest wrong one available.
func TestAutoOnceStillSwitchesWhenTheClaimIsFree(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)

	code, _, errOut, _ := runRoot(t, "auto", "--once")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s), want 0", code, errOut)
	}
	if got := liveUUIDOf(t); got != "u-2" {
		t.Errorf("live account = %q, want u-2", got)
	}
}

// An attended switch is never refused for this — a human typed it and is
// watching — but it says what is about to happen to it.
func TestAttendedSwitchWarnsAboutAnotherStoresEngine(t *testing.T) {
	isolate(t)
	seedAccount(t, "u-1", "a@example.com")
	seedAccount(t, "u-2", "b@example.com")
	foreignClaim(t, "/other/ccdad")

	code, _, errOut, top := runRoot(t, "switch", "2")
	if code != ExitOK {
		t.Fatalf("exit = %d (%s / %s), want 0 — an attended switch is not refused for this", code, errOut, top)
	}
	if got := liveUUIDOf(t); got != "u-2" {
		t.Fatalf("live account = %q, want u-2; the switch did not happen", got)
	}
	if !strings.Contains(errOut, "/other/ccdad") {
		t.Errorf("the switch said nothing about the engine that will undo it:\n%s", errOut)
	}
}

// A probe that cannot answer must not disable auto-start. The daemon degrades
// on an unlockable credential home and takes the singleton, which ends the
// spawn loop by itself — so refusing here would remove the convenience from
// every machine with a network home to guard nothing.
func TestAutoStartStillSpawnsWhenTheClaimCannotBeProbed(t *testing.T) {
	isolate(t)
	enableAutoStart(t)
	f := stubDaemonWorld(t, &fakeDaemon{})
	restore := credhome.SetTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, os.ErrPermission
	})
	t.Cleanup(restore)

	if _, _, _, _ = runRoot(t, "list"); f.spawns != 1 {
		t.Fatalf("spawns = %d, want 1 — an unanswerable probe is not a refusal", f.spawns)
	}
}

// "Nothing to uninstall" is an exit-3 claim that the machine is clean. It must
// see the credential-home files, or a user whose store is already gone is told
// there is nothing left while two of ccdad's files sit in Claude Code's
// directory with no command that mentions them again.
func TestUninstallDoesNotCallAMachineCleanWhileItsClaimFilesRemain(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	// A binary the package manager owns, so there is nothing this command may
	// remove — the same fixture TestUninstallOnAMachineWithNoStoreIsExitThree
	// uses to reach the "nothing to uninstall" answer.
	t.Setenv("HOMEBREW_PREFIX", "/opt/homebrew")
	stubExecutable(t, "/opt/homebrew/bin/ccdad")
	// No store, no removable binary, no PATH entry — the state that produces
	// "nothing to uninstall" — but the claim files are there.
	dir := filepath.Join(mustPath(credhome.Home()), credhome.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, credhome.LockFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr, _ := runRoot(t, "uninstall", "--yes")
	// ExitOK, not merely "not 3". A command that failed for an unrelated reason
	// is also not 3, and asserting the negative would let that pass as a fix.
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 — there was work to do and it should have been done:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, dir) {
		t.Errorf("uninstall never mentioned %s:\n%s", dir, stderr)
	}
}

// The continuous form's refusal, which is a different code path from
// `auto --once`: this one takes the claim rather than probing it inside the
// executor, and its exit code is the one a supervisor sees.
func TestAutoRefusesACredentialHomeAnotherStoreDrives(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)
	foreignClaim(t, "/other/ccdad")

	code, _, stderr, _ := runRoot(t, "auto")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d — 3 is the code operators are told to ignore, so it would "+
			"make this the one state nobody hears about", code, ExitBlocked)
	}
	if !strings.Contains(stderr, "/other/ccdad") {
		t.Errorf("the refusal does not name the store that holds the login:\n%s", stderr)
	}
}

// And the degraded form: a credential home that cannot be locked does not stop
// `ccdad auto`, it only removes the guarantee — which has to be said out loud,
// because nothing else on the machine reports it.
func TestAutoRunsUnguardedWhenTheClaimCannotBeTaken(t *testing.T) {
	isolate(t)
	twoAccountsOneBetter(t)
	restore := credhome.SetTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, errors.ErrUnsupported
	})
	t.Cleanup(restore)

	// runAutoLoop directly, with a deadline, the way the other continuous-form
	// tests drive it: runRoot would block until an interrupt that never comes.
	root := NewRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = runAutoLoop(ctx, newAutoEmitter(root, false), st, time.Millisecond)

	if code := CodeFor(err); code == ExitBlocked {
		t.Fatalf("exit = %d (%v) — an unlockable credential home is a degraded mode, not a refusal:\n%s",
			code, err, errOut.String())
	}
	if !strings.Contains(errOut.String(), "without the credential-home claim") {
		t.Errorf("`ccdad auto` ran unguarded without saying so:\n%s", errOut.String())
	}
	// And it kept working. A degraded mode that stopped switching would be a
	// refusal wearing a notice.
	if got := liveUUIDOf(t); got != "u-2" {
		t.Errorf("live account = %q, want u-2 — the degraded loop never acted", got)
	}
}
