package scripts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// `claude plugin validate --strict` is the authority on the manifest schema and
// nothing here repeats it: a schema written in this repository would be this
// repository's belief about somebody else's schema, and it would drift with
// nothing to say so.
//
// What validate does not do is follow a path. Measured against Claude Code
// 2.1.241: a plugin.json whose mcpServers names a file that is not there passes
// --strict with exit 0; a server entry with no command passes; a marketplace
// source naming a directory that is not there passes with nothing validated at
// all; and an entry name disagreeing with plugin.json's passes. Every assertion
// below is one of those gaps. None of them is a schema assertion.

func repoPath(t *testing.T, parts ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{".."}, parts...)...))
	if err != nil {
		t.Fatalf("resolving %v: %v", parts, err)
	}
	return p
}

func readManifest(t *testing.T, into any, parts ...string) {
	t.Helper()
	path := repoPath(t, parts...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
}

type marketplaceDoc struct {
	Name    string `json:"name"`
	Plugins []struct {
		Name        string `json:"name"`
		Source      string `json:"source"`
		Description string `json:"description"`
		Version     string `json:"version"`
	} `json:"plugins"`
}

type pluginDoc struct {
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	MCPServers string          `json:"mcpServers"`
	Author     json.RawMessage `json:"author"`
}

// A source naming a directory that is not there passes --strict with nothing
// validated, so this is the only reader of that field.
func TestTheMarketplaceSourcePointsAtAPluginThatIsReallyThere(t *testing.T) {
	var m marketplaceDoc
	readManifest(t, &m, ".claude-plugin", "marketplace.json")
	if len(m.Plugins) != 1 {
		t.Fatalf("the marketplace lists %d plugins, want exactly 1", len(m.Plugins))
	}
	dir := repoPath(t, filepath.FromSlash(m.Plugins[0].Source))
	manifest := filepath.Join(dir, ".claude-plugin", "plugin.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("the marketplace source %q has no .claude-plugin/plugin.json under it (%v); "+
			"validate --strict passes a source that is not there, with nothing validated",
			m.Plugins[0].Source, err)
	}
	if m.Plugins[0].Description == "" {
		t.Error("the marketplace entry carries no description; --strict fails without one")
	}
}

// An entry name that disagrees with plugin.json's passes --strict. Install then
// resolves by plugin.json's name and the marketplace's copy names nothing.
func TestTheMarketplaceEntryNamesThePluginThePluginCallsItself(t *testing.T) {
	var m marketplaceDoc
	var p pluginDoc
	readManifest(t, &m, ".claude-plugin", "marketplace.json")
	readManifest(t, &p, "plugins", ".claude-plugin", "plugin.json")
	if m.Plugins[0].Name != p.Name {
		t.Fatalf("the marketplace calls it %q and plugin.json calls it %q; the installed key is "+
			"<plugin>@<marketplace>, so a disagreement here renames the tools", m.Plugins[0].Name, p.Name)
	}
}

// The entry carries no version TODAY, and the assertion has to say both halves:
// a for-all over an absent field is vacuously true, so without the first check
// this test would keep passing after somebody added a version that disagrees.
func TestTheMarketplaceEntryCarriesNoVersionAndWouldHaveToMatchIfItDid(t *testing.T) {
	var m marketplaceDoc
	var p pluginDoc
	readManifest(t, &m, ".claude-plugin", "marketplace.json")
	readManifest(t, &p, "plugins", ".claude-plugin", "plugin.json")
	if m.Plugins[0].Version == "" {
		if p.Version == "" {
			t.Fatal("plugin.json carries no version; the plugin has to name one")
		}
		return
	}
	if m.Plugins[0].Version != p.Version {
		t.Fatalf("the marketplace entry says version %q and plugin.json says %q; --strict fails "+
			"this with a two-file diff, and plugin.json wins at install time regardless",
			m.Plugins[0].Version, p.Version)
	}
}

// --strict does not open the file mcpServers names. A mistyped filename ships a
// plugin that installs, reports enabled, and declares no server at all.
func TestPluginJSONNamesAnMCPFileThatExistsAndDeclaresOneServer(t *testing.T) {
	var p pluginDoc
	readManifest(t, &p, "plugins", ".claude-plugin", "plugin.json")
	if p.MCPServers == "" {
		t.Fatal("plugin.json names no mcpServers file; an inline object reports MCP servers (0) " +
			"in the plugin UI while the server actually runs, which is why the file form is used")
	}
	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	readManifest(t, &doc, "plugins", filepath.FromSlash(p.MCPServers))
	if len(doc.MCPServers) != 1 {
		t.Fatalf("%s declares %d servers, want exactly 1", p.MCPServers, len(doc.MCPServers))
	}
	s, ok := doc.MCPServers["ccdad"]
	if !ok {
		t.Fatalf("%s declares no server named ccdad; the plugin tool names are built from it", p.MCPServers)
	}
	if s.Type != "stdio" {
		t.Errorf("server type is %q, want stdio", s.Type)
	}
	if s.Command != "ccdad" {
		t.Errorf("server command is %q, want the bare name ccdad: Claude Code de-duplicates plugin "+
			"and file-scope servers by endpoint, so an absolute path here runs a SECOND ccdad "+
			"server beside the one ccdad mcp install registers", s.Command)
	}
	if len(s.Args) != 1 || s.Args[0] != "mcp" {
		t.Errorf("server args are %v, want [mcp] -- the endpoint is what the de-duplication keys on", s.Args)
	}
	if s.Env == nil {
		t.Error("server env is absent; it is written even when empty so the bytes match what " +
			"claude mcp add writes and a user diffing the file sees nothing unusual")
	}
}

// A string author fails validate in BOTH modes, not only --strict, so this is
// the one manifest mistake a developer meets before CI does.
func TestPluginJSONCarriesAnAuthorObjectRatherThanAnAuthorString(t *testing.T) {
	var p pluginDoc
	readManifest(t, &p, "plugins", ".claude-plugin", "plugin.json")
	var author struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(p.Author, &author); err != nil {
		t.Fatalf("plugin.json author is not an object (%v); a string author is a hard error in "+
			"both validate modes", err)
	}
	if author.Name == "" {
		t.Error("plugin.json author has no name; --strict requires it")
	}
}

// A marketplace-only key in plugin.json is tolerated at runtime, warned about,
// and therefore fatal under --strict.
func TestNoMarketplaceOnlyKeyAppearsInPluginJSON(t *testing.T) {
	var raw map[string]json.RawMessage
	readManifest(t, &raw, "plugins", ".claude-plugin", "plugin.json")
	for _, k := range []string{"category", "strict", "tags", "source"} {
		if _, ok := raw[k]; ok {
			t.Errorf("plugin.json carries the marketplace-only key %q; it is warned about at "+
				"runtime and fatal under --strict", k)
		}
	}
}
