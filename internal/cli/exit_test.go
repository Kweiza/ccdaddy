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
