package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The configured binary is the escape hatch for a machine where the walk
// cannot get the right answer -- a codex that is not on PATH at all, or two of
// them where the first is not the one wanted -- so it has to WIN rather than
// be a fallback.
func TestTheConfiguredCodexBinaryWinsOverThePathWalk(t *testing.T) {
	isolate(t)
	elsewhere := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(elsewhere, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, "[codex]\nbinary = "+quoteTOML(elsewhere)+"\n")
	saved := resolveCodex
	t.Cleanup(func() { resolveCodex = saved })
	resolveCodex = func(string) (string, error) {
		t.Error("codexBinary walked PATH although [codex] binary names a codex")
		return "", errNoCodex
	}

	got, err := codexBinary()
	if err != nil {
		t.Fatalf("codexBinary = %v", err)
	}
	if got != elsewhere {
		t.Errorf("codexBinary = %q, want the configured %q", got, elsewhere)
	}
}

// A configured path that is not there is a usage error and not a silent
// fallback to PATH: the user said which codex to run, and running a different
// one would bill a session through a binary they did not choose.
func TestAConfiguredCodexBinaryThatIsNotThereIsAUsageError(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "nope", "codex")
	writeConfig(t, "[codex]\nbinary = "+quoteTOML(missing)+"\n")

	_, err := codexBinary()
	if err == nil {
		t.Fatal("codexBinary accepted a configured path that does not exist")
	}
	if !IsUsageError(err) {
		t.Errorf("codexBinary = %v, want a usage error", err)
	}
	if !strings.Contains(err.Error(), "codex.binary") {
		t.Errorf("the error does not name the key to fix: %v", err)
	}
}

// quoteTOML is a basic TOML string. The paths here are t.TempDir()s, which hold
// no quote or backslash on any platform this runs on, so nothing more is needed
// and anything more would be a second TOML encoder in the test suite.
func quoteTOML(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }
