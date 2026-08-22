package daemon

import "fmt"

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
// the daemon rather than a slot in a pid table, and §10.3's image-name and
// creation-time cross-check guards the TerminateProcess fallback behind it.
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
