// Package daemon owns ccdad's background process: where its files live, the
// singleton that decides whether one is running, and the detached spawn that
// starts one.
//
// §8.1's layout is three files with no overlap in how they are used, and the
// split is not stylistic. Windows LockFileEx locks are MANDATORY rather than
// advisory, so a read overlapping a range another handle holds exclusively
// fails with ERROR_LOCK_VIOLATION. The universal Unix idiom — lock the pidfile,
// then write the pid into it — therefore inverts on Windows: a second process
// reading the pidfile gets a hard error, and naive handling reads that as "no
// daemon" and sends a supervisor into a respawn loop.
//
//	~/.ccdad/ccdad.lock    locked exclusively for process life; never written
//	                       (0 bytes forever), never read — try-locked only
//	~/.ccdad/ccdad.pid     never locked; truncate-then-write with a trailing
//	                       newline; read freely
//	~/.ccdad/status.json   never locked; temp + atomic rename per tick; read
//	                       freely
//
// daemon.log is a fourth file the spec's table does not name. It is never
// locked and never read by the daemon itself; it exists here so the layout
// stays in one place, which is what `ccdad doctor` checks for drift against.
package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The four basenames are exported so a reader that needs the NAME and not the
// path — `ccdad uninstall`, deciding whether a directory is a ccdad store —
// gets it from the package that owns it without resolving a home directory it
// does not need.
const (
	LockFileName   = "ccdad.lock"
	PIDFileName    = "ccdad.pid"
	StatusFileName = "status.json"
	LogFileName    = "daemon.log"
)

// LockPath is the singleton lock. It is never written and never read; the only
// operation on it is a try-lock, and it is NEVER unlinked — flock is per-inode,
// so delete-and-recreate lets two daemons each hold "the" lock on a different
// inode, and unlinking also erases the missing-file evidence that no daemon has
// ever started here.
func LockPath() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, LockFileName), nil
}

// PIDPath is the pidfile. It is never locked — see the package comment for why
// locking it inverts on Windows.
func PIDPath() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, PIDFileName), nil
}

// StatusPath is the per-tick status document.
func StatusPath() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, StatusFileName), nil
}

// LogPath is the daemon's log. It is never locked, and the daemon opens it
// itself rather than inheriting it from whoever spawned it — a parent-opened
// descriptor would survive a rename and leave the daemon writing into the
// rotated inode while the new file stays empty.
func LogPath() (string, error) {
	root, err := ccpath.StoreHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, LogFileName), nil
}

// storeRoot resolves the store directory and refuses a relative one.
//
// A relative root is worse here than anywhere else in the tree: a detached
// daemon's working directory differs from its parent's by design, so a relative
// lock path means the daemon flocks one file and the CLI probes another. Two
// invocations from two directories would each see "no daemon" and each spawn
// one. ccpath.StoreHome reports an unresolvable home itself; what reaches this
// guard is a CCDAD_HOME that is relative.
//
// The accessors above only propagate ccpath's error, matching usage.CachePath
// and strategy.StatePath; the absolute-path guard lives on the paths that act,
// exactly as store.Open's does. Canonicalizing with EvalSymlinks — so that two spellings
// of the same CCDAD_HOME cannot yield two daemons — is a separate question and
// belongs to the autostart task that raised it.
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
