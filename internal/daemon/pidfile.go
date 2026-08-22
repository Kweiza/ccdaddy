package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

// pidFilePerm is 0600 to match every other file ccdad writes into its store.
const pidFilePerm = 0o600

// commitMarker is the trailing byte that makes a pidfile body complete.
const commitMarker = '\n'

// WritePID records a pid, in place, as truncate-then-write with a trailing
// newline.
//
// It deliberately does NOT go through cclink.WriteFileAtomic, and that is the
// whole design rather than an oversight. The trailing newline is a commit
// marker, and a commit marker only earns its keep because the write is
// truncate-then-write: with a temp file and a rename there is no torn state for
// a reader to catch, the marker becomes dead code, and the next reader of this
// file deletes it as noise. Both halves have to stay, or neither means
// anything.
//
// For the same reason there is no fsync. A sync would not make a torn write
// whole — it would only decide when the torn bytes reach the platter — and the
// marker already makes a torn write self-identifying to every reader.
func WritePID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to record pid %d", pid)
	}
	root, err := storeRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("creating the ccdad store: %w", err)
	}
	body := strconv.Itoa(pid) + string(commitMarker)
	if err := os.WriteFile(PIDPath(), []byte(body), pidFilePerm); err != nil {
		return fmt.Errorf("writing the pidfile: %w", err)
	}
	return nil
}

// ReadPID reports the recorded pid.
//
// ok is false, with no error, for every state that is a legitimate "there is
// nothing to read here":
//
//   - the file is absent — no daemon has ever run against this store, which is
//     the same genuine evidence the singleton's missing lock file carries;
//   - the file is zero bytes — a write is in flight, and this is the most
//     likely torn state of all, because truncation is the first thing WritePID
//     does;
//   - the body has no trailing newline — a write is in flight and this is the
//     half-written prefix of it.
//
// Everything else is an error, deliberately, including a body that IS
// committed but does not parse. Folding corruption into "nothing to read"
// reproduces one layer down the exact hazard the singleton contract forbids: a
// supervisor cannot tell "no daemon" from "this store is damaged", so it
// respawns forever. `ccdad doctor` is the reader that needs to see it.
//
// A returned pid is never liveness evidence. Only the singleton lock is. The
// process may have died and the number may have been recycled onto something
// unrelated, and Kill(pid, 0) answering proves only that SOME process has that
// pid, never that it is ours.
func ReadPID() (pid int, ok bool, err error) {
	body, err := os.ReadFile(PIDPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("reading the pidfile: %w", err)
	}
	// The length check is not defensive tidiness: it is the zero-byte case,
	// which indexing the last byte would panic on.
	if len(body) == 0 || body[len(body)-1] != commitMarker {
		return 0, false, nil
	}
	text := string(body[:len(body)-1])
	parsed, err := strconv.Atoi(text)
	if err != nil {
		return 0, false, fmt.Errorf("the pidfile holds %q, which is not a pid", text)
	}
	// A corrupt file that parses to 0 or a negative number is worse than
	// useless: Kill(0, SIGTERM) signals ccdad's own process group and Kill(-1)
	// signals every process the user is allowed to signal.
	if parsed <= 0 {
		return 0, false, fmt.Errorf("the pidfile holds %d, which is not a pid", parsed)
	}
	return parsed, true, nil
}

// ClearPID empties the pidfile without removing it.
//
// Removal is never correct. An absent pidfile means "no daemon has ever run
// against this store", and a shutdown that unlinks it forges that state — the
// same reason the singleton lock file is never unlinked. A zero-byte file says
// "a daemon ran here and is not running now", which is the truth.
func ClearPID() error {
	f, err := os.OpenFile(PIDPath(), os.O_WRONLY|os.O_TRUNC, pidFilePerm)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("clearing the pidfile: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("clearing the pidfile: %w", err)
	}
	return nil
}
