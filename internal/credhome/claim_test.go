package credhome

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAcquireTakesTheClaimAndNamesTheHolder(t *testing.T) {
	store, claude := isolate(t)
	c := acquireForTest(t)

	if c.OwnerErr != nil {
		t.Fatalf("OwnerErr = %v, want nil", c.OwnerErr)
	}
	if !Held() {
		t.Error("Held() = false while this process holds the claim")
	}

	body, err := os.ReadFile(filepath.Join(claude, DirName, OwnerFileName))
	if err != nil {
		t.Fatal(err)
	}
	// The trailing newline is a commit marker, not formatting. Asserted here so
	// that a later switch to an encoder that does not emit one fails HERE
	// rather than by making every reader report a torn document.
	if len(body) == 0 || body[len(body)-1] != commitMarker {
		t.Fatalf("the owner document is %q, want a trailing %q", body, string(commitMarker))
	}
	var got Owner
	if err := json.Unmarshal(body[:len(body)-1], &got); err != nil {
		t.Fatal(err)
	}
	if got.Store != store {
		t.Errorf("owner.Store = %q, want %q", got.Store, store)
	}
	if got.PID != os.Getpid() {
		t.Errorf("owner.PID = %d, want %d", got.PID, os.Getpid())
	}
	if got.SchemaVersion != OwnerSchemaVersion {
		t.Errorf("owner.SchemaVersion = %d, want %d", got.SchemaVersion, OwnerSchemaVersion)
	}
	if got.ClaimedAt.IsZero() {
		t.Error("owner.ClaimedAt is zero")
	}
}

// The whole point of the package, and it only exists across processes.
func TestAnotherStoreIsRefusedAndNamed(t *testing.T) {
	_, claude := isolate(t)
	// A DIFFERENT store, the same credential home. That combination is the
	// hazard: two singletons that never meet, one credentials file.
	other := filepath.Join(t.TempDir(), "other-ccdad")
	holdFromAnotherProcess(t, "CCDAD_HOME="+other)

	_, err := Acquire()
	if !errors.Is(err, ErrClaimed) {
		t.Fatalf("Acquire() = %v, want ErrClaimed", err)
	}
	// The store, not just the sentinel. A refusal that does not say WHICH store
	// leaves the user with two terminals and no way to tell which one to fix,
	// and this is the only place that information exists.
	if !strings.Contains(err.Error(), other) {
		t.Errorf("Acquire() = %q, want it to name the holding store %q", err, other)
	}
	if Held() {
		t.Error("Held() = true after a refused Acquire")
	}
	// A refusal must not leave this process believing it holds anything, and
	// must not have written our name over the holder's.
	body, _ := os.ReadFile(filepath.Join(claude, DirName, OwnerFileName))
	if strings.Contains(string(body), `"pid":`+strconv.Itoa(os.Getpid())) {
		t.Errorf("the owner document names this process after a refused Acquire: %s", body)
	}
}

// A second acquire in ONE process must not accuse a stranger. flock denies a
// process its own exclusive lock through a second descriptor, so without the
// in-process guard this reports ErrClaimed while naming our own store and pid —
// an error message that sends the reader looking for a terminal that does not
// exist.
func TestASecondAcquireInOneProcessIsNotBlamedOnAnother(t *testing.T) {
	isolate(t)
	acquireForTest(t)

	_, err := Acquire()
	if !errors.Is(err, ErrAlreadyHeld) {
		t.Fatalf("the second Acquire() = %v, want ErrAlreadyHeld", err)
	}
	if errors.Is(err, ErrClaimed) {
		t.Error("the second Acquire() reported ErrClaimed, which accuses another store of holding our own claim")
	}
}

// doctor and the auto-start hook both probe on the hot path, and §8.2's rule is
// that a probe must never create what it measures: a lock file that exists is
// evidence an engine ran here, and manufacturing it destroys that evidence for
// good.
//
// The credential home is DELETED first. Every fixture in this tree pre-creates
// it, so an assertion written against the ordinary fixture could only ever pass.
func TestProbeCreatesNothing(t *testing.T) {
	_, claude := isolate(t)
	if err := os.RemoveAll(claude); err != nil {
		t.Fatal(err)
	}

	s, err := Probe()
	if err != nil {
		t.Fatalf("Probe() = %v, want nil for a credential home that does not exist", err)
	}
	if s.Held {
		t.Error("Probe() reported a held claim in a directory that does not exist")
	}
	if _, err := os.Stat(claude); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Probe created the credential home %s", claude)
	}

	// And again with the directory present but never claimed, which is the
	// state a machine is in after Claude Code has run and ccdad has not.
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Probe(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(claude, DirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Probe created %s inside the credential home", DirName)
	}

	// And the case that makes the assertion capable of failing at all. With the
	// directory ABSENT, a probe that asked for O_CREATE still creates nothing —
	// the open fails ENOENT and the state looks identical — so both branches
	// above pass against the mutation they exist to catch. The lock file is only
	// creatable once its directory is there.
	if err := os.MkdirAll(filepath.Join(claude, DirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Probe(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(claude, DirName, LockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Probe created %s — the one piece of evidence that says no engine has ever "+
			"claimed this credential home", LockFileName)
	}
}

// The release closure is the only thing keeping the *flock.Flock reachable:
// os.File carries a finalizer that closes the descriptor, and flock releases on
// last close. internal/daemon measured this on its own singleton and needed
// three GC cycles; the anchor in this package exists for it.
//
// Asserted from a SECOND process, because this one cannot tell its own lock
// from a stranger's.
func TestTheClaimSurvivesGarbageCollection(t *testing.T) {
	isolate(t)
	// The returned *Claim is DROPPED, and acquireForTest is deliberately not
	// used: its cleanup closure captures the claim, which keeps the *Flock
	// reachable on its own and makes the package anchor untestable — the test
	// would then pass against the anchor being deleted.
	//
	// So the only reference left is heldClaim itself, and the cleanup reads it
	// back at cleanup time rather than capturing it now.
	if _, err := Acquire(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		heldMu.Lock()
		c := heldClaim
		heldMu.Unlock()
		if err := c.Release(); err != nil {
			t.Errorf("Release() = %v", err)
		}
	})

	for i := 0; i < 3; i++ {
		runtime.GC()
	}

	other := filepath.Join(t.TempDir(), "other-ccdad")
	out, err := runHolderExpectingRefusal(t, "CCDAD_HOME="+other)
	if err == nil {
		t.Fatalf("a second process took the claim after %d GC cycles, so the anchor is not holding it", 3)
	}
	if !strings.Contains(out, ErrClaimed.Error()) {
		t.Errorf("the second process failed with %q, want the ErrClaimed refusal — it may have failed for "+
			"an unrelated reason, which would make this test pass while proving nothing", out)
	}
}

// Release clears the owner document BEFORE unlocking, and the order is the
// whole property. Unlock-first passes any "release works" test and fails this
// one: the incoming holder writes its name into the gap and the departing one
// then blanks it, leaving the new engine unidentifiable for its whole life.
func TestReleaseClearsTheOwnerBeforeUnlocking(t *testing.T) {
	_, claude := isolate(t)
	ownerPath := filepath.Join(claude, DirName, OwnerFileName)

	// The property is an ORDER, so the observation has to be made AT the
	// unlock. Acquiring, releasing and then looking at the file proves nothing:
	// both orders leave the same bytes behind once both steps have run, which
	// is why the obvious version of this test passes against the bug.
	//
	// The seam gives us the unlock itself. Whatever the document holds when the
	// unlock is called is what a second engine acquiring in that instant would
	// go on to overwrite — or, in the broken order, would have overwritten and
	// then had blanked.
	var atUnlock []byte
	restore := SetTryLockForTest(func(string, bool) (bool, func() error, error) {
		return true, func() error {
			atUnlock, _ = os.ReadFile(ownerPath)
			return nil
		}, nil
	})
	defer restore()

	c, err := Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Release(); err != nil {
		t.Fatal(err)
	}

	if len(atUnlock) != 0 {
		t.Errorf("the owner document still said %q at the moment the lock was released; an engine "+
			"acquiring in that gap writes its own name and this one then blanks it, leaving the new "+
			"holder unidentifiable for its whole life", atUnlock)
	}
}

// And the outcome across processes, which is what the ordering is FOR.
func TestTheNextHolderIsTheOneNamedAfterAHandover(t *testing.T) {
	_, claude := isolate(t)
	ownerPath := filepath.Join(claude, DirName, OwnerFileName)

	c, err := Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Release(); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(t.TempDir(), "other-ccdad")
	holdFromAnotherProcess(t, "CCDAD_HOME="+other)

	s, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Held {
		t.Fatal("Probe() reported no holder while a second process holds the claim")
	}
	if !s.Named {
		body, _ := os.ReadFile(ownerPath)
		t.Fatalf("the new holder could not be named (%v); the owner document is %q", s.OwnerErr, body)
	}
	if s.Owner.Store != other {
		t.Errorf("owner.Store = %q, want the NEW holder %q", s.Owner.Store, other)
	}
	if s.Ours {
		t.Error("Probe() reported the other store's claim as ours")
	}
}

// A second process running against the SAME store is the same engine as far as
// this exclusion is concerned. Reporting it as foreign would make an attended
// `ccdad switch` warn about the user's own daemon on every invocation.
func TestAClaimHeldBySameStoreIsOurs(t *testing.T) {
	store, _ := isolate(t)
	holdFromAnotherProcess(t, "CCDAD_HOME="+store)

	s, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Held || !s.Ours {
		t.Fatalf("Probe() = {Held:%v Ours:%v}, want both true for a holder running against our own store", s.Held, s.Ours)
	}
	if v := Decide(); v.StandDown {
		t.Error("Decide() stood down against this store's own engine")
	}
}

// ccdad manufactures two spellings of one store itself: daemon.ChildEnv pins a
// symlink-RESOLVED CCDAD_HOME into every daemon it spawns, while ccpath.StoreHome
// hands back whatever the shell said. A string comparison reports a user's own
// daemon as a foreign engine, and the failure is invisible on a machine with no
// symlink in the path.
func TestOursSurvivesTwoSpellingsOfOneStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows needs a privilege this suite does not assume")
	}
	_, _ = isolate(t)

	real := filepath.Join(t.TempDir(), "real-store")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked-store")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	// The holder spells it one way...
	holdFromAnotherProcess(t, "CCDAD_HOME="+real)
	// ...and we spell the same store the other way.
	t.Setenv("CCDAD_HOME", link)

	s, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Held {
		t.Fatal("Probe() reported no holder")
	}
	if !s.Ours {
		t.Errorf("Probe() reported %q and %q as different stores; they are one directory reached two ways",
			link, s.Owner.Store)
	}
}

func TestDecideStandsDownOnlyAgainstAnotherStore(t *testing.T) {
	isolate(t)
	other := filepath.Join(t.TempDir(), "other-ccdad")
	holdFromAnotherProcess(t, "CCDAD_HOME="+other)

	v := Decide()
	if !v.StandDown {
		t.Fatal("Decide() = proceed while another store's engine holds the claim")
	}
	if v.Owner.Store != other {
		t.Errorf("Decide().Owner.Store = %q, want %q", v.Owner.Store, other)
	}
	if v.Notice != "" {
		t.Errorf("Decide() carried both a stand-down and a notice (%q); a notice is what it says when it "+
			"could NOT tell, and carrying both makes the caller print an excuse for a definite answer", v.Notice)
	}
}

// A held claim whose document cannot be read is "held by somebody I cannot
// name", and an unattended writer PROCEEDS through it with a notice.
//
// Standing down there would be a cron line that silently stops switching
// accounts against a holder nobody can identify — unreportable, and produced by
// a state that is transient by construction (the window between an engine
// taking the claim and writing its name).
func TestDecideFailsOpenAgainstANamelessHolder(t *testing.T) {
	_, claude := isolate(t)
	other := filepath.Join(t.TempDir(), "other-ccdad")
	holdFromAnotherProcess(t, "CCDAD_HOME="+other)

	// Torn: a body with no commit marker is the half-written prefix of a write.
	if err := os.WriteFile(filepath.Join(claude, DirName, OwnerFileName), []byte(`{"schemaVer`), 0o600); err != nil {
		t.Fatal(err)
	}

	v := Decide()
	if v.StandDown {
		t.Fatal("Decide() stood down against a holder it could not name")
	}
	if v.Notice == "" {
		t.Error("Decide() proceeded silently past a holder it could not name; the notice is the only " +
			"trace an operator would ever get")
	}
}

// The two filesystems are independent axes — a local store with a network
// ~/.claude is the ordinary shared-home shape — so this must stay tellable from
// every other probe failure. `ccdad doctor` picks different words for it, and
// the daemon degrades rather than refusing on it.
func TestAFilesystemThatCannotLockIsClassified(t *testing.T) {
	isolate(t)
	restore := SetTryLockForTest(func(string, bool) (bool, func() error, error) {
		return false, nil, errors.ErrUnsupported
	})
	defer restore()

	if _, err := Probe(); !errors.Is(err, ErrLocksUnsupported) {
		t.Errorf("Probe() = %v, want ErrLocksUnsupported", err)
	}
	if _, err := Acquire(); !errors.Is(err, ErrLocksUnsupported) {
		t.Errorf("Acquire() = %v, want ErrLocksUnsupported", err)
	}
	if v := Decide(); v.StandDown {
		t.Error("Decide() stood down on a filesystem that cannot lock; it cannot know that, and refusing " +
			"there takes ccdad away from every machine with a network home")
	} else if v.Notice == "" {
		t.Error("Decide() said nothing about a filesystem that cannot lock")
	}
}

// A relative credential home is the store's own hazard one axis over: a
// detached daemon's working directory differs from its parent's by design, so
// the daemon would flock one file while the CLI probed another.
func TestARelativeCredentialHomeIsRefusedAndNamesTheVariable(t *testing.T) {
	isolate(t)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "relative-claude")

	_, err := Home()
	if err == nil {
		t.Fatal("Home() = nil error for a relative credential home")
	}
	// The VARIABLE, not just the path. Two variables can produce this value and
	// an operator told the wrong one edits the wrong line.
	if !strings.Contains(err.Error(), "CLAUDE_SECURESTORAGE_CONFIG_DIR") {
		t.Errorf("Home() = %q, want it to name CLAUDE_SECURESTORAGE_CONFIG_DIR", err)
	}
	if strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR to an absolute") {
		t.Errorf("Home() = %q, but CLAUDE_CONFIG_DIR is not what produced the relative value", err)
	}

	// And the other way round: with the stronger variable undefined, the same
	// relative value comes from CLAUDE_CONFIG_DIR and must be attributed there.
	os.Unsetenv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	t.Setenv("CLAUDE_CONFIG_DIR", "relative-claude")
	_, err = Home()
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR") {
		t.Errorf("Home() = %v, want it to name CLAUDE_CONFIG_DIR", err)
	}

	// The branch that is easy to get wrong, and the one worth a case of its
	// own: a DEFINED-but-empty CLAUDE_SECURESTORAGE_CONFIG_DIR resolves to
	// ~/.claude and never reads CLAUDE_CONFIG_DIR at all. Naming
	// CLAUDE_CONFIG_DIR there sends an operator to make a variable absolute
	// that already is and is not being consulted — after which the identical
	// error comes back.
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "/absolute/and/irrelevant")
	t.Setenv("HOME", "relative-home")
	t.Setenv("USERPROFILE", "relative-home")
	_, err = Home()
	if err == nil {
		t.Fatal("Home() = nil error for a relative HOME")
	}
	if strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR") {
		t.Errorf("Home() = %q, but CLAUDE_CONFIG_DIR is absolute and is not read on this branch", err)
	}
	if !strings.Contains(err.Error(), "HOME") {
		t.Errorf("Home() = %q, want it to name HOME", err)
	}
}

// SamePath is the answer to a question ccdad creates for itself: it pins a
// resolved, absolute spelling into every daemon it spawns while handing the
// shell's own spelling back everywhere else.
func TestSamePathSeesThroughTheSpellingsCcdadManufactures(t *testing.T) {
	dir := t.TempDir()
	for _, other := range []string{
		dir + string(filepath.Separator),
		filepath.Join(dir, "."),
		filepath.Join(dir, "sub", ".."),
	} {
		if !SamePath(dir, other) {
			t.Errorf("SamePath(%q, %q) = false; they are one directory", dir, other)
		}
	}
	if SamePath(dir, t.TempDir()) {
		t.Error("SamePath reported two different directories as one")
	}
	// The fallback, for paths that cannot be statted: still normalised, still
	// not a bare string compare.
	missing := filepath.Join(dir, "never-created")
	if !SamePath(missing, missing+string(filepath.Separator)) {
		t.Error("SamePath cannot see through a trailing separator when neither path exists")
	}
}

// Neither file is ever unlinked. flock is per-inode, so a release that removed
// the lock file would let two engines each hold "the" claim on a different one —
// the exact state this package exists to prevent.
func TestReleaseUnlinksNothing(t *testing.T) {
	_, claude := isolate(t)
	c, err := Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Release(); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{LockFileName, OwnerFileName} {
		if _, err := os.Stat(filepath.Join(claude, DirName, name)); err != nil {
			t.Errorf("Release removed %s: %v", name, err)
		}
	}
	// Cleared, not removed: a zero-byte document says "an engine was here and
	// is not now", which is the truth. An absent one would say no engine ever
	// claimed this home.
	body, err := os.ReadFile(filepath.Join(claude, DirName, OwnerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Errorf("the owner document is %q after Release, want it empty", body)
	}
}

// The holder answers "mine" from memory, never from the file it wrote.
//
// This is the branch that keeps a daemon from standing its own switches down.
// Without the in-process authority the holder would have to read its own name
// off disk — and a torn write, a truncation, or a file somebody deleted would
// each turn into the holder deciding a stranger was driving its login.
//
// The document is DESTROYED on purpose. A test that left it intact would pass
// against an implementation that reads it, which is the implementation this
// exists to rule out.
func TestTheHolderKnowsTheClaimIsItsOwnWithoutReadingTheDocument(t *testing.T) {
	_, claude := isolate(t)
	acquireForTest(t)

	if err := os.Remove(filepath.Join(claude, DirName, OwnerFileName)); err != nil {
		t.Fatal(err)
	}

	s, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Held {
		t.Fatal("Probe() reported no holder while this process holds the claim")
	}
	if s.Named {
		t.Fatal("the fixture did not take effect: the owner document is still readable")
	}
	if !s.Ours {
		t.Error("the holder did not recognise its own claim once its document was gone; it is " +
			"learning its identity from the file rather than from memory")
	}
	if v := Decide(); v.StandDown {
		t.Error("the holder stood down against itself")
	}
}

// A claim it could not name itself in is still a claim. Giving it back because
// the label failed would trade the exclusion — the thing that actually matters —
// for the description of it.
func TestAClaimSurvivesAnOwnerDocumentItCouldNotWrite(t *testing.T) {
	_, claude := isolate(t)
	// A DIRECTORY where the document goes: the write fails, the flock does not.
	if err := os.MkdirAll(filepath.Join(claude, DirName, OwnerFileName), 0o700); err != nil {
		t.Fatal(err)
	}

	c, err := Acquire()
	if err != nil {
		t.Fatalf("Acquire() = %v, want the claim despite an unwritable owner document", err)
	}
	t.Cleanup(func() { _ = c.Release() })
	if c.OwnerErr == nil {
		t.Error("OwnerErr is nil though the owner document could not be written; the caller has " +
			"nothing to report and the claim looks fully formed")
	}
	if !Held() {
		t.Error("the claim was given up because its label could not be written")
	}
}

// §8.2's window, on this lock. A probe momentarily OWNS the lock it reads, so an
// engine starting alongside `ccdad doctor` or an auto-start hook would lose a
// race it should win without the retry.
//
// Pinned against LITERALS. `acquireAttempts > 1` passes for any value including
// the one that collapses the window the constants exist to create — the same
// trap internal/daemon's singleton test records.
func TestTheAcquireRetryWindowIsWhatItClaims(t *testing.T) {
	if acquireAttempts != 3 {
		t.Errorf("acquireAttempts = %d, want 3", acquireAttempts)
	}
	if acquireRetryDelay != 100*time.Millisecond {
		t.Errorf("acquireRetryDelay = %s, want 100ms", acquireRetryDelay)
	}
}

// And the retry is actually taken, rather than the constants merely existing.
func TestAcquireRetriesBeforeGivingUp(t *testing.T) {
	isolate(t)
	calls := 0
	restore := SetTryLockForTest(func(string, bool) (bool, func() error, error) {
		calls++
		return false, nil, nil
	})
	defer restore()

	if _, err := Acquire(); !errors.Is(err, ErrClaimed) {
		t.Fatalf("Acquire() = %v, want ErrClaimed", err)
	}
	if calls != acquireAttempts {
		t.Errorf("Acquire tried %d times, want %d — a single attempt loses to a passing probe", calls, acquireAttempts)
	}
}
