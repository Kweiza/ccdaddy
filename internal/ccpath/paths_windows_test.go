//go:build windows

package ccpath

import (
	"path/filepath"
	"testing"
)

// THE WHOLE REASON LayoutHome EXISTS, and the one platform where it can be
// shown. Claude Code resolves the home under its config paths as os.homedir()
// and the home under its LAYOUT paths as `env.HOME ?? os.homedir()`. Node's
// os.homedir() prefers %USERPROFILE% on Windows, so the two part the moment
// HOME is set — which a Git-for-Windows shell does by default, on the machine
// most likely to be running a native claude.exe.
//
// Off Windows this cannot fail: Go's os.UserHomeDir IS $HOME there, so both
// resolvers return the same string no matter what the test sets. paths_unix_test
// pins that agreement rather than pretending to test this.
func TestTheTwoHomesPartWhenHOMEIsSet(t *testing.T) {
	profile := t.TempDir()
	home := t.TempDir()
	t.Setenv("USERPROFILE", profile)
	t.Setenv("HOME", home)

	got, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	if got != profile {
		t.Errorf("Home() = %q, want %%USERPROFILE%% %q — the config and credential paths mirror os.homedir()",
			got, profile)
	}

	layout, err := LayoutHome()
	if err != nil {
		t.Fatal(err)
	}
	if layout != home {
		t.Errorf("LayoutHome() = %q, want $HOME %q — the native launcher hangs off lAi()'s home, which reads "+
			"HOME first", layout, home)
	}
}

// The consequence, spelled out where a reader will meet it: .credentials.json is
// under one home and claude.exe under the other. Resolving either through the
// wrong one is silent — a real directory, the wrong account or no install found.
func TestTheCredentialHomeFollowsUSERPROFILEAndNotHOME(t *testing.T) {
	profile := t.TempDir()
	home := t.TempDir()
	t.Setenv("USERPROFILE", profile)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")

	got, err := CredentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(profile, ".claude", CredentialsFile); got != want {
		t.Fatalf("CredentialsPath() = %q, want %q — a switch that wrote under $HOME would leave the login "+
			"claude actually reads untouched", got, want)
	}
}

// An empty HOME is a divergence from Claude Code, taken deliberately: `??` falls
// through on null and undefined but not on "", so Claude Code with HOME="" joins
// a RELATIVE .local/bin. This package's rule is an error or an absolute path,
// never a relative one, so the empty value falls through to %USERPROFILE%.
func TestAnEmptyHOMEFallsThroughRatherThanBecomingARelativePath(t *testing.T) {
	profile := t.TempDir()
	t.Setenv("USERPROFILE", profile)
	t.Setenv("HOME", "")

	got, err := LayoutHome()
	if err != nil {
		t.Fatal(err)
	}
	if got != profile {
		t.Fatalf("LayoutHome() = %q, want %q", got, profile)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("LayoutHome() = %q, which is relative — every path built on it would follow the working "+
			"directory", got)
	}
}

// With NEITHER variable set there is no home to spell. LayoutHome must error
// rather than return "", which is the rule the package header states and the
// one JavaScript's `??` does not keep.
func TestLayoutHomeErrorsWhenNeitherVariableIsSet(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	got, err := LayoutHome()
	if err == nil {
		t.Fatalf("LayoutHome() = %q with no HOME and no USERPROFILE, want an error", got)
	}
	if got != "" {
		t.Errorf("LayoutHome() returned %q alongside its error", got)
	}
}
