// Package credhome is the exclusion that stops two ccdad stores from driving
// one Claude Code login.
//
// ccdad's own state lives under CCDAD_HOME. The login it manages lives under
// CLAUDE_SECURESTORAGE_CONFIG_DIR ?? CLAUDE_CONFIG_DIR ?? ~/.claude. Those are
// independent axes, and the daemon singleton is keyed on the FIRST of them —
// so two shells with genuinely different CCDAD_HOME values each take their own
// singleton, each rank their own accounts, and each write the SAME
// .credentials.json. cclock's protocol serialises the writes, so the file is
// never corrupt; what the two engines fight over is which account is logged in,
// and every switch one makes looks to the other like an external change to
// react to. Nothing else in the tree can see that state, because no process can
// enumerate another's CCDAD_HOME.
//
// So the claim lives beside the file being contended, which is the only place
// two stores can both find it:
//
//	<credential home>/.ccdad/engine.lock    flock'd exclusively for the engine's
//	                                        process life; never written (0 bytes
//	                                        forever), never read, never unlinked
//	<credential home>/.ccdad/engine.owner   never locked; truncate-then-write
//	                                        with a trailing newline; read freely
//
// # Why two files
//
// The lock answers "is an engine driving this login" and the owner document
// answers "which one". They cannot be one file. Windows LockFileEx locks are
// MANDATORY rather than advisory, and gofrs/flock takes its lock over byte
// range [0,1) — so a reader would fail at exactly the offset the document
// begins at. internal/daemon's ccdad.lock/ccdad.pid split exists for the same
// reason, and that package's doc comment spells it out.
//
// The LOCK is the only liveness authority. The owner document is never liveness
// evidence — the rule pidfile.go states for the recorded pid — and it is read
// only once the lock has been observed HELD. That is what makes the crash path
// safe: the kernel drops the flock when the holder's last descriptor closes,
// and the stale document left behind is never consulted again.
//
// # Why a package of its own
//
// internal/daemon already imports internal/switcher (tick.go), and the switch
// executor is one of the callers that has to ask this question — so a claim
// living in internal/daemon would not compile. This is a leaf: ccpath, the lock
// library, and nothing else in the tree.
//
// The try-lock primitive in lock.go is a third copy of one internal/daemon and
// internal/store each own already, and that is deliberate. store/lock.go
// records the rule in writing: each package owns its own lock file, and
// coupling packages in either direction to save twenty lines puts one behind
// the other's build graph. ErrLocksUnsupported is this package's own sentinel
// for the same reason store's is its own.
//
// # A subdirectory, not two dotfiles
//
// `ccdad run --full-profile` seeds a profile by copying every non-DIRECTORY
// entry of the config home (run.go's seedProfile), and a profile IS itself a
// credential home. Loose files would therefore be copied into every profile,
// and each profile would be born holding a forged owner document naming an
// engine that is not driving it. Skipping directories is unconditional there,
// so nesting closes that structurally rather than through an exclusion list a
// later rename can break.
//
// # What this does not cover
//
// It blocks a second engine PROCESS that takes the claim. Two `ccdad auto
// --once` runs that merely probe a free lock can still interleave, and a
// same-store `auto --once` running alongside that store's own daemon is
// knowingly out of scope — that is the store singleton's axis, and auto.go
// says why it is left open.
package credhome

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The three names are exported so a reader that needs the NAME and not the path
// — `ccdad uninstall`, enumerating what it will leave behind — gets it from the
// package that owns it without resolving a home directory it does not need.
const (
	// DirName is ccdad's own directory inside Claude Code's credential home.
	DirName = ".ccdad"
	// LockFileName is the claim itself. Never written, never read, never
	// unlinked.
	LockFileName = "engine.lock"
	// OwnerFileName names the engine holding the claim. Never locked.
	OwnerFileName = "engine.owner"
)

// filePerm matches the rest of what ccdad writes. §10.3: Windows gets no chmod
// and the inherited profile ACL is what protects these, so nothing may depend
// on it there — which is also why doctor's check on these files reports no mode.
const filePerm = 0o600

// Home is Claude Code's credential home, and it refuses a relative one.
//
// ccpath.CredentialHome returns the raw value of whichever variable it read, so
// the guard has to live here. A relative credential home is the same hazard the
// store's guard names in internal/daemon/paths.go, one axis over: a detached
// daemon's working directory differs from its parent's by design, so a relative
// path means the daemon flocks one file while the CLI probes another, and each
// invocation from each directory sees a free claim and takes it.
//
// The message names the variable that actually produced the value rather than
// listing both. An operator who set one of them is being told which one to fix;
// naming the other as well is how a fix gets applied to the wrong line.
func Home() (string, error) {
	home, err := ccpath.CredentialHome()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf(
			"Claude Code's credential home resolved to the relative path %q; set %s to an absolute path",
			home, relativeHomeSource())
	}
	return home, nil
}

// relativeHomeSource names what produced a relative credential home.
//
// It mirrors ccpath.CredentialHome's own order of resolution and must keep
// doing so — BRANCH FOR BRANCH, not merely in the common case. A DEFINED-but-
// empty CLAUDE_SECURESTORAGE_CONFIG_DIR resolves to ~/.claude and stops there,
// so on that branch CLAUDE_CONFIG_DIR is never read at all: half-mirroring it,
// by disqualifying the empty value and then falling through, names a variable
// that is already absolute and is not being consulted, and the same error comes
// straight back after the fix it demanded.
func relativeHomeSource() string {
	if v, ok := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); ok {
		if v != "" {
			return "CLAUDE_SECURESTORAGE_CONFIG_DIR"
		}
		// DEFINED-but-empty resolves to ~/.claude and stops there — it never
		// reads CLAUDE_CONFIG_DIR. Falling through to that branch would tell an
		// operator to make a variable absolute that is already absolute and is
		// not being read, and the error would come straight back after the fix
		// it asked for.
		return "HOME"
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return "CLAUDE_CONFIG_DIR"
	}
	// Neither override is set, so the value came from the home directory
	// itself. os.UserHomeDir errors rather than returning an empty string, so
	// reaching here means $HOME (or %USERPROFILE%) is set to a relative path.
	return "HOME"
}

// Dir is ccdad's directory inside the credential home.
func Dir() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, DirName), nil
}

// LockPath is the claim. The only operation on it is a try-lock, and it is
// NEVER unlinked: flock is per-inode, so delete-and-recreate lets two engines
// each hold "the" claim on a different inode — which is precisely the state
// this package exists to prevent. Unlinking also erases the missing-file
// evidence that no engine has ever claimed this credential home.
func LockPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, LockFileName), nil
}

// OwnerPath is the owner document. It is never locked — see the package comment
// for why locking it would make it unreadable on Windows, which is the one
// platform where reading it matters most.
func OwnerPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, OwnerFileName), nil
}

// storeRoot is the store this engine belongs to, refused when relative for the
// same reason internal/daemon's storeRoot refuses one.
//
// It is resolved here rather than passed in so that no caller can record a
// store it is not actually running against — the owner document is the only
// evidence another engine will ever have about who holds the claim, and a
// caller-supplied value is a caller-supplied forgery.
func storeRoot() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("the ccdad store resolved to the relative path %q; set CCDAD_HOME to an absolute path", root)
	}
	return root, nil
}
