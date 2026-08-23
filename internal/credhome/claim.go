package credhome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrClaimed reports that another store's engine is already driving this
// credential home. It is a sentinel so a caller can tell it from a filesystem
// that cannot lock, which is not a refusal here — see ErrLocksUnsupported.
var ErrClaimed = errors.New("another ccdad engine is already driving this credential home")

// ErrAlreadyHeld reports that THIS process already holds the claim.
//
// It is distinct from ErrClaimed and has to be. flock(2) is per open file
// description, so a second acquire in one process fails exactly as a foreign
// one does — (false, nil), with nothing to tell them apart — and reporting it
// as ErrClaimed would print an accusation naming our own store and our own pid.
var ErrAlreadyHeld = errors.New("this process already holds the credential-home claim")

// Claim is a held claim on a credential home. It is released when the process
// exits, by the kernel, which is the property that makes it the sole authority
// on whether an engine is driving this login: no staleness heuristic can be
// wrong about it.
type Claim struct {
	release   func() error
	ownerPath string
	owner     Owner
	// OwnerErr is why the holder could not write its own name, when it could
	// not. The claim is still HELD in that case, and giving it back because we
	// could not describe ourselves would trade the guarantee for the label. The
	// caller logs it; `ccdad doctor` reports the resulting nameless claim.
	OwnerErr error
}

// heldMu guards heldClaim, which exists for two reasons.
//
// The first is reachability. defaultTryLock explains it: the release closure is
// the only thing keeping the *flock.Flock alive, os.File's finalizer closes the
// descriptor, and flock releases on last close — so an unreachable claim is
// silently dropped. internal/daemon measured that with three runtime.GC()
// cycles on its own singleton.
//
// The second is that this is the IN-PROCESS AUTHORITY on "is this claim mine".
// Without it the holder would have to learn its own identity by reading a file
// it wrote, and every torn or missing write would turn into a wrong answer
// about itself — a daemon that stands its own switches down because it could
// not read its own name.
//
// It is deliberately not shared with any other lock's anchor: a second acquire
// must report ErrAlreadyHeld about THIS lock, not about the store singleton.
var (
	heldMu    sync.Mutex
	heldClaim *Claim
)

// Held reports whether this process holds the claim.
func Held() bool {
	heldMu.Lock()
	defer heldMu.Unlock()
	return heldClaim != nil
}

// Acquire takes the claim on this credential home, or reports why it could not.
//
// It retries first: a probe momentarily OWNS the lock it reads, so an engine
// starting alongside `ccdad doctor` or an auto-start hook can lose a race it
// should win.
//
// The lock file is created here and never unlinked — not on release, not by
// uninstall, not by a test teardown. flock is per-inode, so delete-and-recreate
// lets two engines each hold "the" claim on a different one, which is the exact
// state this package exists to prevent.
//
// A claim it could not name itself in is still a claim. OwnerErr carries that.
func Acquire() (*Claim, error) {
	heldMu.Lock()
	defer heldMu.Unlock()
	if heldClaim != nil {
		return nil, fmt.Errorf("%w", ErrAlreadyHeld)
	}

	home, err := Home()
	if err != nil {
		return nil, err
	}
	store, err := storeRoot()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, DirName)
	// The acquire path creates the directory; the probe path must never do so,
	// because a probe that manufactures the directory manufactures the evidence
	// it was asked to read. cclock already creates the credential home itself at
	// this mode, so nothing new is exposed by doing it here.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s inside Claude Code's credential home: %w", DirName, err)
	}

	lockPath := filepath.Join(dir, LockFileName)
	ownerPath := filepath.Join(dir, OwnerFileName)
	for attempt := 1; ; attempt++ {
		locked, release, err := tryLock(lockPath, true)
		if err != nil {
			return nil, classifyLockError(err)
		}
		if locked {
			o := Owner{
				SchemaVersion: OwnerSchemaVersion,
				Store:         store,
				PID:           os.Getpid(),
				ClaimedAt:     time.Now(),
			}
			c := &Claim{release: release, ownerPath: ownerPath, owner: o}
			// Written AFTER the lock and never before: a document naming an
			// engine that does not hold the claim is worse than no document,
			// because the state it describes cannot be checked against anything.
			c.OwnerErr = writeOwner(ownerPath, o)
			heldClaim = c
			return c, nil
		}
		if attempt >= acquireAttempts {
			return nil, claimedBy(home, ownerPath)
		}
		time.Sleep(acquireRetryDelay)
	}
}

// claimedBy builds the refusal, naming the holder when the holder can be named.
func claimedBy(home, ownerPath string) error {
	o, named, err := readOwner(ownerPath)
	switch {
	case named:
		return fmt.Errorf("%w: the engine for the store at %s (pid %d) holds %s. "+
			"Two stores driving one login fight over which account is live; point CLAUDE_CONFIG_DIR "+
			"at a directory of this store's own, or stop that engine",
			ErrClaimed, o.Store, o.PID, home)
	case err != nil:
		return fmt.Errorf("%w: %s is held by an engine that could not be identified (%v)",
			ErrClaimed, home, err)
	default:
		return fmt.Errorf("%w: %s is held by an engine that has not named itself yet", ErrClaimed, home)
	}
}

// Release gives the claim back. It is safe on a nil receiver and safe to call
// twice, so a shutdown path can call it without tracking whether some earlier
// path already did.
//
// It clears the owner document BEFORE unlocking, and the order is the whole
// point. Unlocking first lets the next engine acquire and write its own name in
// the gap, after which this one's clear would zero the INCOMING engine's
// document — leaving the new holder unidentifiable for the rest of its life,
// and every reader of that credential home unable to say who is driving it.
//
// It unlocks; it never removes either file.
func (c *Claim) Release() error {
	if c == nil {
		return nil
	}
	heldMu.Lock()
	defer heldMu.Unlock()
	if c.release == nil {
		return nil
	}
	err := clearOwner(c.ownerPath)
	err = errors.Join(err, c.release())
	c.release = nil
	if heldClaim == c {
		heldClaim = nil
	}
	if err != nil {
		return fmt.Errorf("releasing the credential-home claim: %w", err)
	}
	return nil
}

// Status is what a probe found.
type Status struct {
	// Home is the credential home the rest of this describes.
	Home string
	// Held is whether an engine holds the claim. It is the only liveness fact
	// here, and it comes from the kernel.
	Held bool
	// Named is whether that engine could be identified.
	Named bool
	// Owner is who it is, when Named.
	Owner Owner
	// Ours is whether the holder is this engine — this process, or another
	// process running against the same store.
	Ours bool
	// OwnerErr is why a held claim could not be named, when the reason was a
	// document that exists and does not make sense rather than one that is
	// simply not written yet.
	OwnerErr error
}

// Probe reports whether an engine is driving this credential home.
//
// It never creates the lock file, the owner document or the directory. A
// missing lock file is genuine evidence that no engine has ever claimed this
// credential home, and a probe that manufactures it destroys that evidence
// permanently — the same contract internal/daemon's SingletonHeld keeps, and
// this one is called from `ccdad doctor` and from the auto-start hook, which
// are the two callers that must least be allowed to change what they measure.
//
// A missing credential home answers "not claimed" for the same reason a missing
// lock file does: a directory that does not exist has no engine in it.
//
// An error means the probe could not answer. It is never folded into "not
// claimed": an engine gating on that would take a claim it should not have.
func Probe() (Status, error) {
	var s Status
	home, err := Home()
	if err != nil {
		return s, err
	}
	s.Home = home

	dir := filepath.Join(home, DirName)
	locked, release, err := tryLock(filepath.Join(dir, LockFileName), false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, classifyLockError(err)
	}
	if locked {
		// We took it, so nobody held it. Give it straight back — and report a
		// failure to do so rather than swallowing it, because a probe that keeps
		// the claim locks out the engine it just said was not there.
		if rerr := release(); rerr != nil {
			return s, fmt.Errorf("releasing the credential-home claim after probing it: %w", rerr)
		}
		return s, nil
	}

	s.Held = true
	s.Owner, s.Named, s.OwnerErr = readOwner(filepath.Join(dir, OwnerFileName))
	s.Ours = ours(s)
	return s, nil
}

// ProbeSettled is Probe with the transient state given time to settle.
//
// Held-but-unnamed is transient BY CONSTRUCTION: it is an engine that has taken
// the claim and not yet written its name, or one clearing its name on the way
// out — Release clears before it unlocks, so a probe can land inside that. A
// caller that merely REPORTS the state may use Probe and say what it saw; a
// caller that ACTS on it must not act on a microsecond, and this is for those.
//
// It retries on the same schedule Acquire does, and for the same reason: an
// answer read at one instant is not a description of a machine.
//
// It is deliberately NOT what the auto-start hook uses. That path runs before
// seven commands, must stay cheap, and its worst case is skipping one
// auto-start — where this one's caller is about to print a refusal.
func ProbeSettled() (Status, error) {
	var (
		s   Status
		err error
	)
	for attempt := 1; ; attempt++ {
		s, err = Probe()
		if err != nil || !s.Held || s.Named || s.Ours {
			return s, err
		}
		if attempt >= acquireAttempts {
			return s, nil
		}
		time.Sleep(acquireRetryDelay)
	}
}

// SamePath reports whether two spellings name one directory.
//
// It is exported because ccdad manufactures differing spellings of one path
// itself and more than one caller has to see through that: daemon.ChildEnv pins
// an ABSOLUTE, symlink-resolved CLAUDE_CONFIG_DIR into every daemon it spawns,
// while ccpath hands the shell's own spelling back untouched — so a daemon and
// the doctor asking about it disagree over a trailing slash, a "..", or a
// symlinked component, with neither of them wrong.
func SamePath(a, b string) bool { return sameStore(a, b) }

// ours answers "is this held claim mine", in the one order that is safe.
//
// This process holding it comes FIRST and settles it without touching the
// filesystem. That is not an optimisation: a shared probe fails against our own
// exclusive lock exactly as it fails against a stranger's, so the holder cannot
// tell itself apart by locking — and if it had to read its own name from disk
// instead, a torn write would make it stand its own switches down.
//
// Otherwise it is a question about STORES, not processes: another process
// running against the same store is the same engine as far as this exclusion is
// concerned, and the store singleton is what keeps two of those apart.
func ours(s Status) bool {
	if Held() {
		return true
	}
	if !s.Named {
		return false
	}
	root, err := storeRoot()
	if err != nil {
		return false
	}
	return sameStore(root, s.Owner.Store)
}

// sameStore reports whether two spellings name one store.
//
// os.SameFile is asked first because ccdad manufactures the two spellings
// itself: daemon.ChildEnv pins a SYMLINK-RESOLVED CCDAD_HOME into every daemon
// it spawns, while ccpath.StoreHome hands back whatever the shell said. A
// string comparison therefore reports a user's own daemon as a foreign engine
// on macOS, where /var is a symlink to /private/var, and on Windows, where
// EvalSymlinks also rewrites case and 8.3 short names. os.SameFile settles
// symlinks, case, 8.3, UNC spellings, trailing separators and NFC/NFD in one
// call, because it compares what the filesystem says rather than what the path
// says.
//
// The string comparison is the fallback for when a Stat cannot answer — a store
// that has not been created yet, or one on a filesystem that has just gone
// away. It is normalised as far as it can be, and it is the weaker answer on
// purpose: getting this wrong in the permissive direction lets two engines run,
// so the fallback is only reached when the authoritative test is unavailable.
func sameStore(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	return normalizeStore(a) == normalizeStore(b)
}

func normalizeStore(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		// Windows paths are case-insensitive, and this is the fallback for
		// exactly the case where os.SameFile could not say so.
		return strings.ToLower(p)
	}
	return p
}

// Verdict is what a writer with nobody watching should do about the claim.
//
// There are only two answers, and "could not tell" is not one of them: it is
// folded into Proceed, with a Notice saying so. A silent stand-down against a
// holder that could not be named is a cron line that stops switching accounts
// with nothing in any log to explain it, and the state that produces it — a
// held claim whose document is torn — is transient. The exclusion that actually
// matters is taken by the engine PROCESS at startup; this is the second line,
// for the callers that hold no lock of their own.
type Verdict struct {
	// StandDown is set only when another store's engine demonstrably holds the
	// claim.
	StandDown bool
	// Owner is that engine, when StandDown.
	Owner Owner
	// Notice is why the answer is not definite, when it is not. It never
	// accompanies StandDown; it is always worth reporting.
	Notice string
}

// Decide is the policy Probe's facts feed. Callers that want the facts —
// `ccdad doctor` — call Probe instead.
func Decide() Verdict {
	s, err := Probe()
	switch {
	case errors.Is(err, ErrLocksUnsupported):
		return Verdict{Notice: fmt.Sprintf(
			"the filesystem holding Claude Code's credential home cannot take a lock (%v), so ccdad cannot "+
				"tell whether another store's engine is driving this login", err)}
	case err != nil:
		return Verdict{Notice: fmt.Sprintf(
			"ccdad could not tell whether another store's engine is driving this login: %v", err)}
	case !s.Held, s.Ours:
		return Verdict{}
	case !s.Named:
		notice := fmt.Sprintf("an engine that has not named itself holds %s", s.Home)
		if s.OwnerErr != nil {
			notice = fmt.Sprintf("an engine holds %s and could not be identified (%v)", s.Home, s.OwnerErr)
		}
		return Verdict{Notice: notice + "; proceeding, because standing down against a holder nobody can name is not reportable"}
	default:
		return Verdict{StandDown: true, Owner: s.Owner}
	}
}
