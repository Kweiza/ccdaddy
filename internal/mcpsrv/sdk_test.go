package mcpsrv

import "testing"

// The name is load-bearing in four files that cannot see each other: the
// server's own identity, Claude Code's mcpServers key, the plugin's server
// config, and every tool name a client derives from it. A rename in one of
// those and not the others produces two live servers rather than an error.
func TestTheServerNameIsTheOneClaudeCodeDeDuplicatesOn(t *testing.T) {
	if ServerName != "ccdad" {
		t.Fatalf("ServerName = %q, want %q", ServerName, "ccdad")
	}
}

// The version reaches initialize from the caller rather than from a package
// constant, so a test can pin a fixture without a release moving it.
func TestTheReportedVersionIsWhateverTheCallerPassed(t *testing.T) {
	impl := implementation("0.0.0-test")
	if impl.Name != ServerName {
		t.Errorf("Name = %q, want %q", impl.Name, ServerName)
	}
	if impl.Version != "0.0.0-test" {
		t.Errorf("Version = %q, want the caller's constant", impl.Version)
	}
}
