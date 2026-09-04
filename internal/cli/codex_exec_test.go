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

// The walk is handed the SHIM DIRECTORY, and codexBinary is the only place in
// production that argument comes from. Handed an empty one instead, the walk's
// skip short-circuits and its first answer is <CCDAD_HOME>/bin/codex -- the
// shim, which execs `ccdad codex exec`, which resolves codex again: an
// unbounded loop with a process per turn of it.
//
// The stub CAPTURES its argument rather than discarding it, which is the whole
// point of this test. A stub of shape func(string) (string, error) that ignores
// what it is given is green whatever codexBinary passes, including "".
func TestTheUnconfiguredCodexBinaryHandsTheWalkTheShimDirectory(t *testing.T) {
	isolate(t)
	// No [codex] binary: this is the branch where the walk decides.
	want := shimDir()
	if want == "" {
		t.Fatal("shimDir is empty in this test's environment, so it cannot be told apart from an unskipped walk")
	}
	elsewhere := filepath.Join(t.TempDir(), "codex")
	saved := resolveCodex
	t.Cleanup(func() { resolveCodex = saved })
	var got string
	called := false
	resolveCodex = func(shim string) (string, error) {
		called, got = true, shim
		return elsewhere, nil
	}

	binary, err := codexBinary()
	if err != nil {
		t.Fatalf("codexBinary = %v", err)
	}
	if !called {
		t.Fatal("codexBinary did not walk PATH although no [codex] binary is set")
	}
	if binary != elsewhere {
		t.Errorf("codexBinary = %q, want the walk's answer %q", binary, elsewhere)
	}
	if got != want {
		t.Errorf("the PATH walk was handed %q, want the shim directory %q: a walk that is not told "+
			"which directory to skip answers with the shim itself, and the shim execs "+
			"`ccdad codex exec`, which walks again -- an unbounded process loop", got, want)
	}
}

// quoteTOML is a basic TOML string. The paths here are t.TempDir()s, which hold
// no quote or backslash on any platform this runs on, so nothing more is needed
// and anything more would be a second TOML encoder in the test suite.
func quoteTOML(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

// The measured fact this exists for: an HTTP_PROXY or ALL_PROXY in the
// environment CAPTURES a loopback base_url, and only a BARE `127.0.0.1` entry
// in NO_PROXY exempts it -- `localhost` does not, and `127.0.0.1:<port>` does
// not. Without the exemption every request codex makes to ccdad's own listener
// goes to the user's corporate proxy instead, and the symptom is codex's
// endless "Reconnecting" with no error text at all.
func TestWithNoProxyLoopbackAddsTheBareLoopbackEntry(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		env                  []string
		wantUpper, wantLower string
	}{
		{
			name:      "neither set",
			env:       []string{"HOME=/home/u"},
			wantUpper: "127.0.0.1", wantLower: "127.0.0.1",
		},
		{
			name:      "only the upper-case one is set, and the lower borrows it",
			env:       []string{"NO_PROXY=example.com"},
			wantUpper: "example.com,127.0.0.1", wantLower: "example.com,127.0.0.1",
		},
		{
			name:      "only the lower-case one is set, and the upper borrows it",
			env:       []string{"no_proxy=example.com"},
			wantUpper: "example.com,127.0.0.1", wantLower: "example.com,127.0.0.1",
		},
		{
			name:      "each keeps its own value",
			env:       []string{"NO_PROXY=a.example", "no_proxy=b.example"},
			wantUpper: "a.example,127.0.0.1", wantLower: "b.example,127.0.0.1",
		},
		{
			name:      "a bare entry that is already there is not doubled",
			env:       []string{"NO_PROXY=127.0.0.1,example.com"},
			wantUpper: "127.0.0.1,example.com", wantLower: "127.0.0.1,example.com",
		},
		{
			name:      "a bare entry with spaces around it counts",
			env:       []string{"NO_PROXY=example.com, 127.0.0.1"},
			wantUpper: "example.com, 127.0.0.1", wantLower: "example.com, 127.0.0.1",
		},
		{
			name:      "localhost is NOT the loopback entry",
			env:       []string{"NO_PROXY=localhost"},
			wantUpper: "localhost,127.0.0.1", wantLower: "localhost,127.0.0.1",
		},
		{
			name:      "a host:port entry is NOT the loopback entry",
			env:       []string{"NO_PROXY=127.0.0.1:8080"},
			wantUpper: "127.0.0.1:8080,127.0.0.1", wantLower: "127.0.0.1:8080,127.0.0.1",
		},
		{
			name:      "a trailing comma does not become an empty component",
			env:       []string{"NO_PROXY=example.com,"},
			wantUpper: "example.com,127.0.0.1", wantLower: "example.com,127.0.0.1",
		},
		{
			name:      "an empty value is the same as unset",
			env:       []string{"NO_PROXY=", "no_proxy="},
			wantUpper: "127.0.0.1", wantLower: "127.0.0.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withNoProxyLoopback(tc.env)
			if v := envValueOf(got, "NO_PROXY"); v != tc.wantUpper {
				t.Errorf("NO_PROXY = %q, want %q", v, tc.wantUpper)
			}
			if v := envValueOf(got, "no_proxy"); v != tc.wantLower {
				t.Errorf("no_proxy = %q, want %q", v, tc.wantLower)
			}
		})
	}
}

// Nothing is ever REMOVED. This makes an exception for one host; it does not
// turn the user's proxy off, and a launcher that quietly unset HTTP_PROXY
// would send every request codex makes to anything else -- an npm install, a
// docs fetch -- straight out of a network that requires one.
func TestWithNoProxyLoopbackNeverRemovesAProxyVariable(t *testing.T) {
	env := withNoProxyLoopback([]string{
		"HTTP_PROXY=http://corp:3128",
		"HTTPS_PROXY=http://corp:3128",
		"ALL_PROXY=socks5://corp:1080",
	})
	for _, want := range []string{
		"HTTP_PROXY=http://corp:3128",
		"HTTPS_PROXY=http://corp:3128",
		"ALL_PROXY=socks5://corp:1080",
	} {
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is gone from the child environment: %q", want, env)
		}
	}
}

// setEnv replaces rather than appends, so a variable the parent exported twice
// does not reach the child twice with two different values.
func TestWithNoProxyLoopbackLeavesOneCopyOfEachVariable(t *testing.T) {
	env := withNoProxyLoopback([]string{"NO_PROXY=a", "NO_PROXY=b"})
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "NO_PROXY=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the child environment holds %d copies of NO_PROXY: %q", n, env)
	}
}
