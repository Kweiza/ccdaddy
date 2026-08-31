package daemon

import (
	"fmt"
	"os"
	"strconv"
)

// RecoveryEnvVar carries how many times THIS chain of daemon processes has
// already replaced itself. It travels in the child's environment, the way
// ChildEnvVar does, and for the same reason: it is a fact about one chain of
// processes, and the store is shared with every other daemon that ever ran
// against it.
//
// An absent, unparseable or negative value reads as zero. The variable is not a
// user-facing knob -- nothing documents it and nothing asks anyone to set it --
// so the only sane reading of a value ccdad did not write is "no replacements
// have happened".
const RecoveryEnvVar = "CCDAD_DAEMON_RECOVERY"

// maxRecoveries is how many times a daemon may replace itself before it stops
// trying and keeps running instead.
//
// Three, and the shape of the evidence is why it is small. Replacement is worth
// attempting because it has WORKED: a wedged daemon was replaced and its
// successor was healthy on its first tick. It is bounded because it has also
// FAILED -- an earlier replacement of the same wedged daemon came up and wedged
// again immediately -- so a machine can be in a state a fresh process does not
// fix, and on that machine an unbounded rule is a process spawning a successor
// every five minutes until someone notices.
//
// What the cap buys is that the failure ends somewhere, and where it ends is a
// daemon that is still running and still publishing. That is the state doctor
// can report and a user can act on; the alternative ends in no daemon at all.
const maxRecoveries = 3

// recoveryBudget is how many more times this process may replace itself.
func recoveryBudget() int {
	left := maxRecoveries - recoveriesSoFar()
	if left < 0 {
		return 0
	}
	return left
}

func recoveriesSoFar() int {
	n, err := strconv.Atoi(os.Getenv(RecoveryEnvVar))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// NextRecoveryCount is the number the successor inherits.
//
// A process that PASSED a tick and wedged later starts the successor's count
// over at one, because its own run is evidence that the machine works and the
// wedge is a new event. Only a process that never got a single tick through
// continues the count it was started with -- that is the chain the cap is
// counting, and letting a week of healthy running reset it is the point: a
// daemon that wedges after a week must still get its replacements.
//
// freshChain is that judgement, and a RE-ARM is the second thing that makes it
// true. A loop with no budget left that has stayed wedged past
// recoveryRearmAfter is allowed one more attempt precisely because the machine
// it gave up on may have changed since; handing the successor an already-spent
// count would make that attempt the last one forever, which is the state the
// re-arm exists to leave.
func NextRecoveryCount(soFar int, freshChain bool) int {
	if freshChain {
		return 1
	}
	return soFar + 1
}

// WedgedError is Run reporting a tick loop it gave up on, and what the
// successor should be told.
//
// The count rides on the ERROR rather than being recomputed by the caller
// because only this process knows whether any of its ticks ever passed, and
// that fact is gone by the time Run has returned.
type WedgedError struct {
	// Err is the loop's own error, which names the run and its last cause.
	Err error
	// NextRecovery is the value the successor's RecoveryEnvVar must carry.
	NextRecovery int
}

func (e *WedgedError) Error() string { return e.Err.Error() }
func (e *WedgedError) Unwrap() error { return e.Err }

// SpawnSuccessor starts a replacement daemon carrying the recovery count.
//
// It must be called with the singleton RELEASED -- the successor's first act is
// to take it -- which is why nothing inside Run may call this: Run gives the
// singleton back in a defer, so only its caller runs late enough.
func SpawnSuccessor(next int) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the ccdad binary: %w", err)
	}
	return spawnWithEnv(exe, func(env []string) []string {
		return setEnv(env, RecoveryEnvVar, strconv.Itoa(next))
	})
}
