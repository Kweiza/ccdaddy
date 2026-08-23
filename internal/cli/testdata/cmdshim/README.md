# npm cmd-shim fixtures

These are real npm shims, not hand-written approximations. Each was produced by
running npm's own generator — `cmd-shim` 8.0.0, as bundled with npm 11.8.0 —
over a fixture package laid out the way npm's Windows global prefix is:

    <prefix>/claude.cmd
    <prefix>/node_modules/@anthropic-ai/claude-code/cli.js

They were generated on Linux, and that is not a compromise: `writeShim_` in
`cmd-shim/lib/index.js` never reads `process.platform`, so the bytes it writes
for a `.cmd` are the same on every host. The generator is the source of truth
for the format, which is why the fixtures come from it rather than from a
transcription of one machine's `claude.cmd`.

One file per shebang shape the generator branches on:

| file | the target's first line | what it exercises |
|---|---|---|
| `env-node.cmd` | `#!/usr/bin/env node` | the ordinary case: an interpreter and one script |
| `env-dash-s-flags.cmd` | `#!/usr/bin/env -S node --experimental-vm-modules` | interpreter flags, which must precede the script |
| `env-with-vars.cmd` | `#!/usr/bin/env FOO=bar node` | `@SET` lines the shim exports before running |
| `absolute-node.cmd` | `#!/usr/bin/node` | a POSIX interpreter path the generator emits verbatim, which is not resolvable on Windows |
| `no-shebang.cmd` | none | the target is invoked directly, with no interpreter |

Regenerate with `scripts/gen-cmd-shim-fixtures.js` when npm's template changes.
