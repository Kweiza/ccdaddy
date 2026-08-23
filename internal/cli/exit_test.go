package cli

import (
	"errors"
	"fmt"
	"testing"
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
