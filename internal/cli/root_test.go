package cli

import (
	"bytes"
	"strings"
	"testing"

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

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want an error")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor(unknown command) = %d, want %d", got, ExitUsage)
	}
}
