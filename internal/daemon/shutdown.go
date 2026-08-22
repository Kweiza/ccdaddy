package daemon

import (
	"errors"
	"fmt"
	"os"
)

// ErrNoShutdownListener reports that nothing is listening for a shutdown
// request against this store.
//
// It exists because Windows answers that question directly and Unix cannot:
// OpenEvent returning ERROR_FILE_NOT_FOUND means no daemon ever created the
// event, which is a negative answer to a probe (§9.3's exit 5) rather than a
// failure. A signal sent to a pid has no equivalent — a pid either exists or
// does not, and neither says whether the process is listening.
var ErrNoShutdownListener = errors.New("nothing is listening for a shutdown request")

// RequestShutdown asks the daemon at pid to stop, and returns as soon as the
// request is delivered.
//
// The pid is NOT what makes this safe, and nothing here can make it safe on its
// own: ReadPID's contract says a recorded pid is never liveness evidence, and a
// pid the kernel has recycled belongs to an unrelated process this would
// terminate. The caller's guard is the singleton — `ccdad daemon stop` sends
// this only when SingletonHeld() has just answered yes, which is the one fact
// about a daemon that cannot be stale in the direction that matters. A daemon
// exiting between that probe and this call leaves a window Unix cannot close;
// it is two syscalls wide and it is the window every process supervisor lives
// with. Windows does not have to live with it, because the named event names
// the STORE rather than a slot in a pid table — the pid is not even used there.
//
// Waiting for the daemon to actually go is the caller's job, and it must poll
// the SINGLETON rather than the pid: the kernel releases the lock when the
// process dies, and nothing else on the machine reports that without a race.
func RequestShutdown(pid int) error {
	// The two values a damaged pidfile is likeliest to yield are the two that
	// turn this into an attack on the machine: Kill(0, SIGTERM) signals ccdad's
	// own process group, and Kill(-1, SIGTERM) signals every process the user
	// is allowed to signal. ReadPID refuses to hand either out; this refuses to
	// act on one whatever route it arrived by.
	if pid <= 0 {
		return fmt.Errorf("refusing to signal pid %d", pid)
	}
	return requestShutdown(pid)
}

// ForceShutdown terminates the daemon at pid — and only if the process holding
// that pid is still the daemon that was recorded.
//
// It is the LAST resort and never the first: the caller must have asked
// gracefully and waited. On Unix there is no such thing here at all, and that
// is deliberate rather than unfinished — a daemon ignoring SIGTERM is a bug the
// user has to be told about, and `kill -9` is one command away when they decide
// otherwise. Windows has no equivalent a user can safely reach for: `taskkill
// /F` takes a pid and performs no cross-check whatsoever, which is precisely
// the mistake §10.3 lists this against.
//
// Everything it needs to identify the daemon it recorded is read here rather
// than passed in, so a caller cannot assemble a target that skips a check.
func ForceShutdown(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to terminate pid %d", pid)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the ccdad binary: %w", err)
	}
	target := shutdownTarget{PID: pid, Image: exe}

	// The published document is where the daemon's own start time comes from,
	// and it is the anchor the creation-time check needs. A document describing
	// a DIFFERENT pid is not evidence about this one: refuse rather than
	// terminate on a start time that belongs to another process.
	s, ok, err := ReadStatus()
	if err != nil {
		return fmt.Errorf("reading the daemon's published status to identify pid %d: %w", pid, err)
	}
	if ok {
		if s.PID != pid {
			return fmt.Errorf("refusing to terminate pid %d: %s describes pid %d",
				pid, statusFileName, s.PID)
		}
		target.StartedAt = s.StartedAt
	}
	return forceShutdown(target)
}
