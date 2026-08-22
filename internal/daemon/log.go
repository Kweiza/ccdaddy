package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// daemon.log's size policy. §8.4 says "rotate daemon.log if large", which is not
// a number; these are.
//
// 8 MiB is roughly a week of a chatty daemon at this tick rate, and three kept
// generations bound the whole thing at 32 MiB — a cap that matters because
// nothing ever prunes ~/.ccdad on the user's behalf.
const (
	maxLogSize  = 8 * 1024 * 1024
	keepRotated = 3
)

// logFilePerm matches the rest of the store. §10.3: no chmod on Windows.
const logFilePerm = 0o600

// logTimeFormat is RFC 3339 to the millisecond. A daemon's log is read next to
// `ccdad status` output and next to Claude Code's own, so the timestamps have to
// be sortable and unambiguous rather than pretty.
const logTimeFormat = "2006-01-02T15:04:05.000Z07:00"

// Logger is the daemon's log file.
//
// It is the fourth file in the store and the one §8.1's table deliberately does
// not cover, because it has none of those constraints: it is NEVER locked and
// NEVER read by the daemon itself. `ccdad daemon logs` reads it, and a reader
// competing with a rotation is a reader's problem — nothing here waits for one.
//
// The daemon opens this file ITSELF rather than inheriting a descriptor from
// whoever spawned it, and that is the whole reason Spawn hands the child
// /dev/null on all three descriptors. A parent-opened descriptor survives the
// rename that rotation performs, so the daemon would keep appending to the
// rotated inode while the fresh daemon.log stayed 0 bytes — every line after the
// first rotation silently discarded, forever.
type Logger struct {
	mu   sync.Mutex
	path string
	f    *os.File
	max  int64
	keep int

	// capture records that fd 2 was pointed at this file, so a rotation can
	// point it at the new one. Without that, a panic after the first rotation
	// goes to the inode nothing will ever read again.
	capture bool

	// now is a seam. Log timestamps are the only output of this type, so a test
	// that cannot fix them cannot assert on them.
	now func() time.Time
}

// OpenLog opens the daemon log with the size policy above.
func OpenLog() (*Logger, error) { return openLog(LogPath(), maxLogSize, keepRotated) }

func openLog(path string, max int64, keep int) (*Logger, error) {
	if keep < 1 {
		return nil, fmt.Errorf("the daemon log must keep at least one rotation, not %d", keep)
	}
	root, err := storeRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating the ccdad store: %w", err)
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return &Logger{path: path, f: f, max: max, keep: keep, now: time.Now}, nil
}

// openAppend opens the log for appending, creating it if it is not there.
//
// O_APPEND is what makes concurrent writers safe at the kernel level as well as
// under the mutex: the offset is taken at write time, so a poller goroutine and
// the tick loop cannot land on top of each other.
func openAppend(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFilePerm)
	if err != nil {
		return nil, fmt.Errorf("opening the daemon log: %w", err)
	}
	return f, nil
}

// Printf appends one line.
//
// It reports nothing. A daemon that cannot write its log has no better channel
// to say so on — the alternative is a write error on every line for the rest of
// the process's life, which is noise rather than information.
func (l *Logger) Printf(format string, a ...any) {
	line := l.now().Format(logTimeFormat) + " " + fmt.Sprintf(format, a...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.WriteString(line)
}

// CaptureStderr points file descriptor 2 at the log.
//
// This is not decoration. A panic and a runtime fatal go STRAIGHT to descriptor
// 2 without passing through any logger, and Spawn hands the child /dev/null on
// all three descriptors — so without this, the only trace a crash will ever
// leave is thrown away. §8.3 assigns the job to whoever owns the log, which is
// this type, because rotation has to carry the redirect over to the new file.
func (l *Logger) CaptureStderr() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := redirectStderr(l.f); err != nil {
		return fmt.Errorf("pointing stderr at the daemon log: %w", err)
	}
	l.capture = true
	return nil
}

// RotateIfLarge rotates when the log has grown past the cap, and reports whether
// it did.
func (l *Logger) RotateIfLarge() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	info, err := l.f.Stat()
	if err != nil {
		return false, fmt.Errorf("measuring the daemon log: %w", err)
	}
	if info.Size() < l.max {
		return false, nil
	}
	if err := l.rotate(); err != nil {
		return false, err
	}
	return true, nil
}

// rotate shifts the generations and reopens. The caller holds the mutex.
//
// The descriptor is closed BEFORE the renames rather than relying on
// share-delete semantics. Go's os.OpenFile does pass FILE_SHARE_DELETE, so a
// rename under an open handle works on Windows too — but closing first means the
// rotation does not depend on that, and the reopen is required regardless.
func (l *Logger) rotate() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("closing the daemon log to rotate it: %w", err)
	}
	// Oldest first, or each generation overwrites its neighbour on the way past.
	// The oldest is dropped by being renamed OVER — os.Rename replaces an
	// existing file on every platform ccdad targets, Windows included, where Go
	// passes MOVEFILE_REPLACE_EXISTING. An explicit Remove of generation `keep`
	// stood here until a mutation showed nothing could observe it: no name past
	// `keep` is ever written, so the count is bounded by construction.
	for i := l.keep - 1; i >= 1; i-- {
		err := os.Rename(l.rotatedPath(i), l.rotatedPath(i+1))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("shifting rotated logs: %w", err)
		}
	}
	if err := os.Rename(l.path, l.rotatedPath(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotating the daemon log: %w", err)
	}

	f, err := openAppend(l.path)
	if err != nil {
		// The daemon is now without a log. Leave the field nil-free by keeping
		// the closed handle out: every Printf from here on writes to a closed
		// descriptor and fails silently, which is the documented behaviour, and
		// the caller gets a real error to log through whatever is left.
		return err
	}
	l.f = f
	if l.capture {
		if err := redirectStderr(f); err != nil {
			return fmt.Errorf("re-pointing stderr at the rotated daemon log: %w", err)
		}
	}
	return nil
}

func (l *Logger) rotatedPath(i int) string { return l.path + "." + strconv.Itoa(i) }

// Close releases the file. It does not restore stderr: the process is on its way
// out, and a daemon whose last act is to redirect its own crash output away from
// the log is a daemon whose crash nobody sees.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("closing the daemon log: %w", err)
	}
	return nil
}
