package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// Claude Code registers an MCP server twice over, by two routes that look
// interchangeable and are not.
//
// A file-scope entry -- what `ccdad mcp install` writes -- de-duplicates by
// NAME, per scope. A plugin's server de-duplicates by ENDPOINT: the command
// plus its arguments. Measured, a file-scope entry whose command is `ccdad mcp`
// makes the plugin's identical server VANISH from the listing, and renaming the
// file-scope entry changes nothing. That is the good case: both name the same
// endpoint, so one server runs. Change one byte of the command and both run,
// which is two full tool sets and two credential-rotation primitives on one
// machine.
//
// The half that breaks quietly is the tool names. A plugin's tools are
// mcp__plugin_<plugin>_<server>__<tool>; a file-scope server's are
// mcp__<server>__<tool>. A permission rule, hook matcher or allowed-tools entry
// written for one never fires under the other, with no error and no log line.

// pluginInstall is one record out of Claude Code's own plugin registry.
type pluginInstall struct {
	// Key is "<plugin>@<marketplace>", verbatim, because the warning names it
	// and a user greps for what they typed.
	Key string
	// Scope is "user", "project" or "local", and empty when the record omits
	// it. All three land in the one registry file.
	Scope string
}

// installedCcdadPlugins reports every installed plugin whose key begins
// "ccdad@".
//
// The prefix rather than the whole key: a marketplace can be added under any
// name, so keying on "ccdad@ccdaddy" would report a clean machine on a machine
// that has the plugin installed from a mirror.
//
// It returns no error. Every caller is adding a sentence to something else's
// output, and a registry belonging to another program -- missing, half-written,
// or newer than this binary understands -- is not a reason for ccdad to fail.
func installedCcdadPlugins() []pluginInstall {
	// ConfigHome rather than the home directory: CLAUDE_CONFIG_DIR moves the
	// whole config root, which is exactly what `ccdad run --full-profile` does,
	// and a hard-coded path would answer this against one account's profile
	// instead of against the machine.
	home, err := ccpath.ConfigHome()
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(home, "plugins", "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Plugins map[string][]struct {
			Scope string `json:"scope"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var out []pluginInstall
	for key, records := range doc.Plugins {
		if !strings.HasPrefix(key, "ccdad@") {
			continue
		}
		if len(records) == 0 {
			out = append(out, pluginInstall{Key: key})
			continue
		}
		for _, r := range records {
			out = append(out, pluginInstall{Key: key, Scope: r.Scope})
		}
	}
	// Sorted, because the map above has no order and a diagnostic that reorders
	// itself between two runs is one nobody can diff.
	sortPluginInstalls(out)
	return out
}

func sortPluginInstalls(in []pluginInstall) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && (in[j].Key < in[j-1].Key ||
			(in[j].Key == in[j-1].Key && in[j].Scope < in[j-1].Scope)); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// warnPluginCollision says what just happened to the tool names.
//
// It warns and does not refuse. Refusing would strand a user who wants a
// project-scope entry, and it would teach nothing: once both entries name the
// endpoint `ccdad mcp`, they cannot both be live, so there is no arrangement in
// which the two coexist and a refusal preserves.
//
// The tool-name divergence gets three of the five lines because it is the half
// that silently breaks configuration a user already wrote. The de-duplication
// half only surprises them once.
func warnPluginCollision(w io.Writer, installs []pluginInstall, scope string) {
	if len(installs) == 0 {
		return
	}
	for _, p := range installs {
		where := p.Scope
		if where == "" {
			where = "unrecorded"
		}
		fmt.Fprintf(w, "ccdad mcp install: the ccdad plugin is already installed (%s, scope: %s).\n", p.Key, where)
	}
	fmt.Fprintf(w, "ccdad mcp install: registered `ccdad` in the %s scope anyway; Claude Code de-dupes MCP servers\n", scope)
	fmt.Fprintln(w, "  by endpoint, so this entry replaces the plugin's copy rather than running a second one.")
	fmt.Fprintln(w, "ccdad mcp install: the tools are now named mcp__ccdad__* — NOT mcp__plugin_ccdad_ccdad__*.")
	fmt.Fprintln(w, "  Any permission rule, hook matcher or allowed-tools entry written for the plugin name will")
	fmt.Fprintln(w, "  stop firing. Run `ccdad mcp uninstall` to hand the server back to the plugin.")
}
