package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The plugin ships one file whose bytes ccdad also prints, and this is the gate
// that keeps them one artifact rather than two that agree today.
//
// The working directory under `go test` is this package's source tree, and
// isolate does not change it, so the repository root is two levels up.
func repoFile(t *testing.T, parts ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{"..", ".."}, parts...)...))
	if err != nil {
		t.Fatalf("resolving %v: %v", parts, err)
	}
	return p
}

func readRepoJSON(t *testing.T, into any, parts ...string) {
	t.Helper()
	raw, err := os.ReadFile(repoFile(t, parts...))
	if err != nil {
		t.Fatalf("reading %v: %v", parts, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%v is not valid JSON: %v", parts, err)
	}
}

func TestPrintConfigIsTheFileThePluginShips(t *testing.T) {
	claude := isolate(t)
	want, err := os.ReadFile(repoFile(t, "plugins", ".mcp.json"))
	if err != nil {
		t.Fatalf("reading the plugin's server file: %v", err)
	}

	code, stdout, stderr, _ := runRoot(t, "mcp", "install", "--print-config")
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, stderr)
	}
	if stdout != string(want) {
		t.Fatalf("--print-config and plugins/.mcp.json have drifted apart.\n"+
			"The file is CAPTURED from this command; regenerate it rather than editing it.\n"+
			"--print-config:\n%q\nplugins/.mcp.json:\n%q", stdout, string(want))
	}
	// --print-config exists for users who refuse on-disk mutation, so it must
	// leave the config home exactly as it found it.
	if _, err := os.Stat(filepath.Join(claude, ".claude.json")); err == nil {
		t.Error("--print-config wrote Claude Code's config; it prints and mutates nothing")
	}
}

func TestThePluginsEndpointNamesACommandThisBinaryActuallyHas(t *testing.T) {
	// A manifest that validates is not a manifest that connects: the validator
	// checks a schema and never asks whether the command exists. This is the
	// check that connects the two halves, and it costs one cobra lookup.
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	readRepoJSON(t, &doc, "plugins", ".mcp.json")

	s := doc.MCPServers["ccdad"]
	if s.Command != "ccdad" {
		t.Fatalf("the plugin's command is %q; an absolute path here stops the plugin entry and the "+
			"one ccdad mcp install writes from de-duplicating, and BOTH servers then run", s.Command)
	}
	cmd, _, err := NewRootCmd().Find(s.Args)
	if err != nil {
		t.Fatalf("the plugin's args %v name no command on this binary: %v", s.Args, err)
	}
	if !cmd.Runnable() {
		t.Fatalf("%q is registered but not runnable, so the plugin's server would exit immediately",
			cmd.CommandPath())
	}
	if cmd.CommandPath() != "ccdad mcp" {
		t.Fatalf("the plugin's args resolve to %q, want ccdad mcp", cmd.CommandPath())
	}
}
