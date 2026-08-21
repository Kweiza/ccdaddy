package cli

import (
	"bytes"
	"strings"
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
