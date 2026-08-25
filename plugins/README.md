# The ccdad plugin

MCP wiring, and nothing else. It registers one stdio MCP server named `ccdad`,
whose command is the `ccdad` binary — so the binary has to be on your PATH. If
it is not, the plugin still installs and still reports as enabled; only
`claude mcp list` says the server failed to connect.

Install ccdad first: https://github.com/Kweiza/ccdaddy

## If you already ran `ccdad mcp install`

That registration wins and the plugin's server will not appear in
`claude mcp list`. Claude Code de-duplicates MCP servers by endpoint — the
command and its arguments — and a file-scope entry outranks a plugin one, so
the two never run side by side once both name `ccdad mcp`.

The two paths do not produce the same tool names:

| Registered by | Tool names |
|---|---|
| `ccdad mcp install` | `mcp__ccdad__switch`, `mcp__ccdad__list`, … |
| this plugin | `mcp__plugin_ccdad_ccdad__switch`, `mcp__plugin_ccdad_ccdad__list`, … |

A permission rule, a hook matcher or an allowed-tools entry written for one
spelling silently never fires under the other. Run `ccdad mcp uninstall` to hand
the server back to the plugin.
