package cli

import (
	"bytes"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
)

func TestRootVersionFlag(t *testing.T) {
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if got := out.String(); !strings.Contains(got, buildinfo.Version) {
		t.Fatalf("--version output %q does not contain version %q", got, buildinfo.Version)
	}
}

func TestRootUnknownCommandIsUsageError(t *testing.T) {
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"definitely-not-a-command"})

	err := ExecuteCmd(cmd)
	if err == nil {
		t.Fatal("ExecuteCmd() = nil, want an error")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor(unknown command) = %d, want %d", got, ExitUsage)
	}
}

func TestRootUnknownSubcommandThroughFind(t *testing.T) {
	root := NewRootCmd()
	// Register a throwaway subcommand so Cobra's Find() actually evaluates unknown
	// command detection. Without a subcommand, Find() returns nil and the test would
	// silently exercise the Task-1 stopgap RunE instead of the normalization path.
	root.AddCommand(&cobra.Command{
		Use:  "add",
		RunE: func(*cobra.Command, []string) error { return nil },
	})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"definitely-not-a-command"})

	err := ExecuteCmd(root)
	if err == nil {
		t.Fatal("ExecuteCmd() = nil, want an error")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor(unknown subcommand via Find()) = %d, want %d", got, ExitUsage)
	}
}

func TestExecuteWithUnknownSubcommandThroughFind(t *testing.T) {
	root := NewRootCmd()
	root.AddCommand(&cobra.Command{
		Use:  "add",
		RunE: func(*cobra.Command, []string) error { return nil },
	})
	root.SetArgs([]string{"bogus-subcommand"})

	var errBuf bytes.Buffer
	code := ExecuteWith(root, &errBuf)
	if code != ExitUsage {
		t.Fatalf("ExecuteWith(unknown subcommand) = %d, want %d", code, ExitUsage)
	}
}

func TestExecuteWithSuccessIsOKAndPrintsNothing(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{"--version"})

	var errBuf bytes.Buffer
	code := ExecuteWith(root, &errBuf)
	if code != ExitOK {
		t.Fatalf("ExecuteWith(--version) = %d, want %d", code, ExitOK)
	}
	if got := errBuf.String(); got != "" {
		t.Fatalf("error buffer = %q, want empty", got)
	}
}

func TestExecuteWithErrorWrittenToErrOut(t *testing.T) {
	root := NewRootCmd()
	root.AddCommand(&cobra.Command{
		Use:  "add",
		RunE: func(*cobra.Command, []string) error { return nil },
	})
	root.SetArgs([]string{"unknown-cmd"})

	var errBuf bytes.Buffer
	code := ExecuteWith(root, &errBuf)
	if code != ExitUsage {
		t.Fatalf("ExecuteWith(unknown) code = %d, want %d", code, ExitUsage)
	}
	if got := errBuf.String(); !strings.Contains(got, "ccdad: ") {
		t.Fatalf("error buffer %q does not contain %q", got, "ccdad: ")
	}
}

func TestExecuteWithEPIPEIsOK(t *testing.T) {
	root := NewRootCmd()
	root.SetArgs([]string{})
	// Replace the RunE to return EPIPE error
	root.RunE = func(*cobra.Command, []string) error {
		return fmt.Errorf("writing output: %w", syscall.EPIPE)
	}

	var errBuf bytes.Buffer
	code := ExecuteWith(root, &errBuf)
	if code != ExitOK {
		t.Fatalf("ExecuteWith(EPIPE) = %d, want %d", code, ExitOK)
	}
	if got := errBuf.String(); got != "" {
		t.Fatalf("error buffer = %q, want empty on EPIPE", got)
	}
}
