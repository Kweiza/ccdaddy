package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/store"
)

func TestCodeForNil(t *testing.T) {
	if got := CodeFor(nil); got != ExitOK {
		t.Fatalf("CodeFor(nil) = %d, want %d", got, ExitOK)
	}
}

func TestCodeForUsageError(t *testing.T) {
	err := UsageError("unknown account %q", "nope")
	if !IsUsageError(err) {
		t.Fatal("IsUsageError = false, want true")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor(usage) = %d, want %d", got, ExitUsage)
	}
	if got, want := err.Error(), `unknown account "nope"`; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestCodeForWrappedUsageError(t *testing.T) {
	err := fmt.Errorf("resolving account: %w", UsageError("bad ref"))
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor(wrapped usage) = %d, want %d", got, ExitUsage)
	}
}

func TestCodeForCodedError(t *testing.T) {
	for _, want := range []ExitCode{ExitNothingToDo, ExitBlocked, ExitProbeNegative} {
		err := WithCode(errors.New("x"), want)
		if got := CodeFor(err); got != want {
			t.Fatalf("CodeFor(coded %d) = %d, want %d", want, got, want)
		}
	}
}

func TestCodeForPlainError(t *testing.T) {
	if got := CodeFor(errors.New("boom")); got != ExitFailure {
		t.Fatalf("CodeFor(plain) = %d, want %d", got, ExitFailure)
	}
}

// The numeric values ARE the contract. Every other assertion in this package
// compares a produced constant against the same constant, which cannot notice a
// renumbering — and the exit contract exists precisely because cswap collapsed
// two meanings onto one number.
func TestExitCodeLiterals(t *testing.T) {
	want := map[ExitCode]int{
		ExitOK: 0, ExitFailure: 1, ExitUsage: 2, ExitNothingToDo: 3,
		ExitBlocked: 4, ExitProbeNegative: 5, ExitInterrupted: 130,
	}
	for code, n := range want {
		if int(code) != n {
			t.Errorf("exit code = %d, want %d", int(code), n)
		}
	}
	// Two codes collapsing onto one number is the cswap defect by name: exit 4
	// says "do something", exit 3 says "ignore me".
	seen := map[int]bool{}
	for code := range want {
		if seen[int(code)] {
			t.Fatalf("two exit codes share the value %d", int(code))
		}
		seen[int(code)] = true
	}
}

// An interrupted store write is the one Ctrl-C in this binary that the shell
// never sees as a signal: the transaction holds SIGINT for the span of its own
// write, so the process is not killed and the exit code has to come from here.
func TestCodeForAnInterruptedStoreWrite(t *testing.T) {
	err := fmt.Errorf("importing accounts: %w", store.ErrInterrupted)
	if got := CodeFor(err); got != ExitInterrupted {
		t.Fatalf("CodeFor(interrupted) = %d, want %d", got, ExitInterrupted)
	}
}

// An explicit tag still wins. A command that has already decided what its own
// interruption means — `add` returns ExitInterrupted for a cancelled login —
// must not have that answer replaced by this mapping, and a command that tags
// something else must not have it replaced either.
func TestAnExplicitCodeOutranksTheInterruptMapping(t *testing.T) {
	err := WithCode(fmt.Errorf("%w", store.ErrInterrupted), ExitNothingToDo)
	if got := CodeFor(err); got != ExitNothingToDo {
		t.Fatalf("CodeFor(coded interrupted) = %d, want %d", got, ExitNothingToDo)
	}
}
