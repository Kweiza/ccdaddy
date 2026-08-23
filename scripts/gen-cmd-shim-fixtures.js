#!/usr/bin/env node
// Regenerates internal/cli/testdata/cmdshim/*.cmd from npm's OWN generator.
//
// The fixtures have to be what npm writes, not a transcription of it, and the
// generator is the only thing that can say so. cmd-shim's writeShim_ never
// reads process.platform, so running it here produces the same bytes it would
// on Windows — which is what makes a Windows-only format testable from a Linux
// machine at all.
//
//   node scripts/gen-cmd-shim-fixtures.js <outdir>
//
// then copy <outdir>/<case>/claude.cmd to internal/cli/testdata/cmdshim/<case>.cmd.
// Requires npm on PATH; the cmd-shim it uses is the one that npm ships with.
const { execFileSync } = require('child_process')
const npmRoot = execFileSync('npm', ['root', '-g'], { encoding: 'utf8' }).trim()
const cmdShim = require(require('path').join(npmRoot, 'npm', 'node_modules', 'cmd-shim'))
const fs = require('fs'), path = require('path')
const out = process.argv[2]

// The Windows global-prefix layout npm actually uses: the shim sits directly in
// the prefix, the package under <prefix>/node_modules.
const cases = [
  ['env-node',        '#!/usr/bin/env node\n'],
  ['env-dash-s-flags','#!/usr/bin/env -S node --experimental-vm-modules\n'],
  ['absolute-node',   '#!/usr/bin/node\n'],
  ['env-with-vars',   '#!/usr/bin/env FOO=bar node\n'],
  ['no-shebang',      'MZ\x90\x00binary\n'],
]
for (const [name, shebang] of cases) {
  const prefix = path.join(out, name)
  const pkg = path.join(prefix, 'node_modules', '@anthropic-ai', 'claude-code')
  fs.mkdirSync(pkg, { recursive: true })
  const from = path.join(pkg, 'cli.js')
  fs.writeFileSync(from, shebang + 'console.log("claude")\n')
  cmdShim(from, path.join(prefix, 'claude')).then(() => {
    process.stdout.write('=== ' + name + ' ===\n')
    process.stdout.write(JSON.stringify(fs.readFileSync(path.join(prefix, 'claude.cmd'), 'utf8')) + '\n')
  })
}
