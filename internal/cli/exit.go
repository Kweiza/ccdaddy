// Package cli holds the ccdad command tree and the process-wide exit contract.
package cli

import (
	"errors"
	"fmt"

	"github.com/Kweiza/ccdaddy/internal/store"
)

// ExitCode is the process exit status. The contract is global: every command
// uses these and only these, so a caller can branch on the code without
// knowing which command produced it.
type ExitCode int

const (
	// ExitOK means the requested action was taken.
	ExitOK ExitCode = 0
	// ExitFailure is a runtime failure: network, I/O, lock contention, token refresh.
	ExitFailure ExitCode = 1
	// ExitUsage is reserved exclusively for usage errors: a bad flag, a bad flag
	// combination, an unknown account reference, a missing argument. Keeping it
	// exclusive is what lets a cron job tell a typo from a no-op.
	ExitUsage ExitCode = 2
	// ExitNothingToDo means the world is already how the caller asked for it.
	ExitNothingToDo ExitCode = 3
	// ExitBlocked means the action was wanted but no viable target exists, so
	// the caller should do something about it. Alert on this; ignore ExitNothingToDo.
	ExitBlocked ExitCode = 4
	// ExitProbeNegative is a negative answer to a probe, not a failure: no daemon
	// is running, nothing is attributable. It exists so a supervisor loop can tell
	// "no daemon" from "cannot determine", which is ExitFailure.
	ExitProbeNegative ExitCode = 5
	// ExitInterrupted is SIGINT.
	ExitInterrupted ExitCode = 130
)

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// UsageError builds an error that maps to ExitUsage.
func UsageError(format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...)}
}

// IsUsageError reports whether err, or anything it wraps, is a usage error.
func IsUsageError(err error) bool {
	var target *usageError
	return errors.As(err, &target)
}

type codedError struct {
	err  error
	code ExitCode
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// WithCode tags err with an explicit exit code.
func WithCode(err error, code ExitCode) error {
	if err == nil {
		return nil
	}
	return &codedError{err: err, code: code}
}

// CodeFor maps an error to its exit code. An explicit tag wins; an interrupted
// store write is next; then a usage error; anything else is a runtime failure.
func CodeFor(err error) ExitCode {
	if err == nil {
		return ExitOK
	}
	var coded *codedError
	if errors.As(err, &coded) {
		return coded.code
	}
	// A store transaction holds SIGINT for the span of its own write, so a
	// Ctrl-C that lands there does not reach the process's default disposition
	// and cannot become 130 the way every other Ctrl-C in this binary does —
	// the shell never sees a signalled process. Mapped here rather than tagged
	// with WithCode at each call site because the transaction is reached from
	// `add`, `import`, `bootstrap`, `remove`, `alias`, `run`, the token refresh
	// and the daemon's tick, and the one that forgot the tag would be the one
	// reporting a cancelled import as a runtime failure.
	//
	// It adds nothing to the contract above: 130 is already ExitInterrupted and
	// already means exactly this. See internal/store/interrupt.go for why only
	// SIGINT is held, and for the two opposite sentences the store attaches
	// depending on which side of the commit the signal landed on.
	if errors.Is(err, store.ErrInterrupted) {
		return ExitInterrupted
	}
	if IsUsageError(err) {
		return ExitUsage
	}
	return ExitFailure
}
