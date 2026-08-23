# Changelog

Notable changes, in the format of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
following [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Per-release notes are also generated on the
[releases page](https://github.com/Kweiza/ccdaddy/releases) from the commit
history; this file is the shorter, human-grouped view, and it is where anything
that would surprise an upgrader gets written down.

While the version is below `1.0.0`, the CLI surface may change between minor
versions. The one thing that is already a promise is the stability contract
`ccdad --help` prints: **`idx` is a display ordinal, not a key.** It is
recompacted whenever an account is removed, so scripts must reference accounts
by `uuid` or `alias`.

## [Unreleased]

### Added

- **One engine per Claude Code login.** `CCDAD_HOME` and `CLAUDE_CONFIG_DIR` are
  independent axes, and moving only the first is the trap: two shells with
  different stores and the same credential root each took their own daemon
  singleton and both rewrote the same `.credentials.json`. Nothing was
  corrupted — the writes are serialised — the two engines simply undid each
  other's switches, and no command anywhere said so. ccdad now takes a second
  exclusion on the CREDENTIAL home (`<credential home>/.ccdad/engine.lock`, with
  `engine.owner` beside it naming the store that holds it, unlocked so it stays
  readable on Windows where locks are mandatory). A second store's daemon
  refuses to start and names the store that has it; `ccdad auto` refuses with
  exit `4`; `ccdad auto --once`, which holds no lock of its own, stands down
  inside the switch executor; auto-start stops spawning children that would
  immediately die. An attended `ccdad switch` is never refused — a human typed
  it — but it says which engine is about to undo it. A filesystem that cannot
  lock the credential home DEGRADES rather than refusing: the engine keeps
  running unguarded, and `ccdad doctor` names that. Neither file is ever
  removed, including by `ccdad uninstall`, because deleting a lock file splits
  the exclusion it provides and another ccdad store may still be using it.

- **`ccdad doctor` gains a `credential-home` check** — the fourteenth. It reports
  which credential home this shell resolves, whether an engine is driving it and
  which store that engine belongs to, and it catches the case nothing else can:
  a running daemon whose recorded credential home differs from the one you
  resolve, which is what a daemon started from inside `ccdad run --full-profile`
  looks like from outside.

- **`ccdad setup-path` puts the binary's directory on your `PATH`.** It is the
  answer to `ccdad: command not found` right after `curl | bash`, which has the
  installer's own script on stdin and so cannot ask permission to edit a startup
  file. It writes a marker-fenced block into the files your shell actually
  reads — for bash that is `~/.bashrc` *and* your login file, because a login
  shell reads only one of them and a terminal-emulator shell reads only the
  other — and running it twice leaves one block. `--print` emits the same bytes
  without writing. Exit `3` means nothing was written; it is keyed on what is
  registered, never on the live `$PATH`, so a directory that is on `$PATH` only
  because you pasted an `export` line still gets a durable registration. On
  Windows there is no startup file: the directory goes into `HKCU\Environment`
  with its value kind preserved and the change is broadcast, which is the same
  write `install.ps1` performs. It never creates `~/.bash_profile` (that would
  stop bash login shells from ever reading `~/.profile` again), and it refuses
  rather than guessing for csh, for an unknown `$SHELL`, and for a Homebrew or
  Scoop install whose `PATH` the package manager owns.

- **Bare `ccdad` is the dashboard, behind a TTY gate.** With a terminal on both
  stdout and stdin it renders exactly what `ccdad status` renders, followed by a
  one-line footer of the top verbs, and it auto-starts the daemon for the same
  reason `ccdad status` does. Anywhere else — a pipe, a redirect, cron — it
  prints usage on stderr and exits `2`, and starts nothing. The slot is promised
  to a TUI, so an answer no script can build on is what keeps that change a
  widening rather than a break. `ccdad -- list`, `ccdad -` and `ccdad ""` are
  usage errors as well: those tokens are dropped before dispatch, so the command
  written after them would never have run.

- **`ccdad switch --model <name>` narrows the ranking.** The weekly caps scoped
  to other models stop counting against an account, so one whose Opus week is
  spent can still be chosen for a Sonnet session. It only ever raises an
  account's headroom, it narrows `consume-first` as well as `headroom`, and a
  model name `ccdad` cannot place is now a usage error rather than a flag that
  silently did nothing.

### Added

- **`ccdad run --full-profile` serves API-key accounts.** Claude Code reads an
  API key from `primaryApiKey` in its global config, not from a credential
  home, so the default mode — which shares that file with the live session on
  purpose — still refuses, and now names the flag that works instead of only
  pointing at `ccdad switch`. Under `--full-profile` the key is written to the
  profile's own global config and nothing outside the profile moves. The
  `ANTHROPIC_API_KEY` route was measured and not taken: it is read outright by
  `claude -p` but gated for an interactive session on an approval list in that
  same config, and `--bare` bypasses the gate — one flag away from a different
  answer.

### Fixed

- **`ccdad run` on Windows no longer refuses an argument npm's shim would have
  mangled.** When `claude` on PATH is npm's `claude.cmd`, `ccdad run acct -p
  'fix&whoami'` was a usage error: Go emits an argument with no space or quote
  RAW, and `cmd.exe` reads the ampersand as a command separator. ccdad now
  reads the shim, and launches the interpreter it names — `node cli.js` —
  directly, which takes `cmd.exe` out of the launch entirely. It does this only
  where the refusal would otherwise fire, so a shim it cannot parse, an
  interpreter that is not installed, or one that itself resolves to a `.cmd`
  all keep the old behaviour rather than failing in a new way.

- **`ccdad switch` typed inside a `ccdad run` session rewrote the session, not
  the live login.** A session is a whole Claude Code, and everything typed in
  it inherits the session's `CLAUDE_SECURESTORAGE_CONFIG_DIR`, so the switch
  wrote `<session>/.credentials.json`, printed `Switched to`, changed nothing
  outside the session, and replaced the session's own login with another
  account's — in a directory `run` deletes on the way out. The commands that
  write Claude Code's state, delete the store the session lives in, or leave a
  daemon behind carrying the session's scope now exit `2` and name the session:
  `switch`, `auto`, `add`, `add-token`, `remove`, `uninstall`, `daemon start`
  and `daemon restart`. Reads still run, and `ccdad doctor` now says which
  session it is inside rather than reporting the session's credentials file as
  the live login.

- **Auto-start could still spawn a daemon inside a `--full-profile` session.**
  The existing guard read `CLAUDE_SECURESTORAGE_CONFIG_DIR`, which that mode
  removes rather than sets — so a daemon born there was pinned to the profile
  and managed it for the rest of its life. An ordinary `CLAUDE_CONFIG_DIR` of
  your own is still not a reason to refuse; only a config home ccdad created
  for a run is.

- **`ccdad export --include-mcp` no longer speaks for the machine from inside a
  session.** Neither when there are none to include, nor — the half that
  matters — when there ARE: it used to write the session's own MCP logins into
  the file and label them "this machine's", so a backup taken from inside a
  session held the wrong secrets under the right name.

- **`ccdad remove` now deletes the account's `--full-profile` profile.** Since
  `ccdad run --full-profile` began writing an API-key account's key into
  `<store>/profiles/<uuid>/.claude.json`, an orphaned profile is an orphaned
  CREDENTIAL rather than stale configuration — and nothing cleaned one:
  `uninstall` removes the whole store and `doctor` scans sessions only. The
  profile is named in the confirmation, because it also holds the MCP logins
  and trust answers that account accumulated.

- **A nested `ccdad run --full-profile` no longer seeds one account's profile
  from another's.** A profile is seeded from `CLAUDE_CONFIG_DIR`, resolved when
  the profile is created — which inside a `--full-profile` session is the outer
  account's profile, not the machine's configuration. Creating a profile there
  copied that account's settings and its `primaryApiKey` into a second
  account's profile, at a path nothing lists and `ccdad remove` does not clean.
  Only the CREATE is refused, so a nested run of an account whose profile
  already exists still works, and a nested run inside a default-mode session —
  which does not redirect `CLAUDE_CONFIG_DIR` — is unaffected.

### Changed

- **`ccdad uninstall` now removes the `PATH` entry ccdad registered**, on both
  platforms. On Windows this was owed before `setup-path` existed:
  `install.ps1` has written `HKCU\Environment\Path` since it shipped, and every
  one-liner install left an entry pointing at a directory uninstall had just
  emptied. What it removes is only what ccdad can prove it added: on Unix that
  is what lies between ccdad's own markers, so a `PATH` line you wrote yourself
  is never touched; on Windows there are no markers, so `setup-path` and
  `install.ps1` now record the directory they added under
  `HKCU\Software\ccdad`, and an entry with no such record is left in place and
  named. That matters for a `go install` or a zip install, where the directory
  is one you put on `PATH` yourself and holds your other tools. A startup file
  whose fence is unterminated or doubled is reported and left alone rather than
  guessed at, and the remaining files are still cleaned.

- **`install.sh` points at `ccdad setup-path`** — by absolute path, because that
  message only appears when the install directory is off `PATH` and a bare
  `ccdad` would not resolve. The `export PATH=…` line is still printed
  underneath, for the shell you are standing in.

- **The per-model and per-surface weekly caps the usage endpoint reports in
  `limits[]` now rank, pace and display.** They were parsed and then ignored, so
  an account whose Fable or Cowork week was gone read as healthy; they now bind
  headroom like any other window, feed `consume-first`, carry a pace reading,
  and appear in `status --json` under `windows`. A cap of any other kind, or one
  whose scope names neither a model nor a surface, is still ignored.
- **A `limits[]` entry with no readable `percent` is unknown rather than 0%.**
  The schema writes the field non-null, but a body that omits it would otherwise
  have read as a window with everything left.

### Fixed

- **`ccdad auto --json` no longer reports a stand-down as a completed switch.**
  The outcome switch had no `default`, so any outcome it did not name fell
  through to the success path and emitted `{"kind":"switched"}` with exit `0` —
  to the one consumer that cannot see the machine. It now names every outcome
  and fails loudly on one it does not know.

- **`ccdad auto` no longer discards a failure to release the daemon singleton.**
  Its release ran in a deferred closure assigning to an UNNAMED return value, so
  the assignment went to a dead local and the error vanished. A lock that could
  not be given back is precisely the error the next invocation trips over.

- **`install.ps1` no longer appends a duplicate `PATH` entry** on a machine
  whose user `PATH` holds `%LOCALAPPDATA%\Programs\ccdad` unexpanded. It reads
  the value raw — correctly, so that `%VAR%` references are not frozen to
  today's expansion — but compared each component only as stored, against a
  fully expanded install directory, so the entry never matched and a second copy
  was appended on every install.

## [0.1.0] — 2026-08-23

The first release the install one-liners can reach: `v0.1.0-rc1` was a
prerelease, so `releases/latest` had no answer for it and both installers
aborted fail-closed. That is fixed by this tag existing, not by any code in it.

### Added

- **The background daemon.** A self-managed detached process with a `flock`
  singleton, a pidfile, a 1 Hz tick loop and a `status.json` publisher, started
  automatically by any `ccdad` command. `ccdad daemon start|stop|status`
  drives it directly.
- **`ccdad auto`** — the auto-switch engine: the poller fleet, the measured
  poll policy with AIMD congestion control, and the swap executor.
- **`ccdad status`** — the engine dashboard, read entirely from disk. It never
  dials out.
- **`ccdad doctor`** — thirteen checks over the layout ccdad depends on: the
  store, permissions, locks, pidfile, status file, usage cache, engine state,
  config, leftover session directories, Claude Code's credential file, its
  top-level keys, a stale legacy macOS keychain item, and the environment
  variables that would make a switch a no-op. It reports; it repairs nothing,
  and it never creates what it is checking for.
- **`ccdad run`** — start a Claude Code session as a chosen account without
  changing the live login, by scoping that session's credential directory.
- **`ccdad list --refresh`** — take a fresh usage reading before listing, where
  the poll policy allows one, and two columns (`LEFT`, `RESETS IN`) to show it.
  The endpoint allows roughly 28–30 requests per identity per rolling hour on a
  sliding window, so a reading under three minutes old is served as it stands
  and no request is made; it says on stderr when it did nothing and why.
- **`ccdad config`** — read and write `~/.ccdad/config.toml`.
- **`ccdad export` / `ccdad import`** — a portable JSON document of the account
  store, with a three-flag gate on including MCP credentials.
- **`ccdad disable`, `enable`, `alias`, `move`** — the remaining account verbs.
- **`ccdad uninstall`** — stops the daemon, deletes the store, removes the
  binary. Both installers already pointed at it.
- **API-key account activation**, modelling Claude Code's own resolution order
  for `ANTHROPIC_API_KEY`, `apiKeyHelper` and `primaryApiKey`.
- **CI**: `gofmt` and `go vet`, `go test -race` on Linux, macOS and Windows,
  and a gate that fails if any of the six release targets needs cgo. The
  release workflow now refuses to publish a tag whose commit has no green CI
  run. Pull requests from forks get their own run; a branch in this repository
  keeps the run its push already triggered.
- **The files a public repository is expected to have**: a README, `LICENSE`,
  `NOTICE`, `THIRD-PARTY-LICENSES.txt`, `SECURITY.md`, `CONTRIBUTING.md`, a
  code of conduct, issue and pull request templates and a dependabot config.
  Every release now also carries `LICENSE`, `NOTICE` and
  `THIRD-PARTY-LICENSES.txt` as assets, hashed into the same `sha256sums.txt`
  and covered by the same attestation as the binaries — BSD-3-Clause and
  Apache-2.0 require the notice to accompany a *binary* distribution.

### Changed

- Every path resolver returns an error instead of an empty string, so an
  unresolvable `$HOME` fails loudly rather than putting a credential store in
  the current directory.
- Store writes take a cross-process lock and re-read inside it.

### Fixed

- A credential rotated by a running session is adopted back only into an
  account that actually has one.
- `ccdad uninstall` no longer leaves behind the per-session directories
  `ccdad run` creates.
- Several Windows paths that no test had ever compiled, let alone run, found by
  putting Windows in the CI matrix.

## [0.1.0-rc1] — 2026-08-22

First published build. **A prerelease**, which means GitHub's
`releases/latest` deliberately has no answer for it: the install one-liners in
the README abort fail-closed until a non-prerelease tag exists. To install this
one, pin it — see the README's *Installing a specific version*.

### Added

- **Accounts and login.** `ccdad add` (browser OAuth with the dual-path
  loopback/paste race), `ccdad add-token`, `ccdad list`, `ccdad which`,
  `ccdad switch`, `ccdad remove`.
- **The credential swap.** Claude Code's three locks taken in its own order,
  the file re-read under the lock, an atomic rename, and a **deny-list** of the
  five account-scoped keys so that machine-scoped keys — `mcpOAuth` above all —
  survive a switch instead of being destroyed.
- **Usage and ranking.** The usage API client with its cache, the headroom and
  pace model, account ranking, the credit gate behind two independent opt-ins,
  and account reclassification.
- **Anti-flap.** Hysteresis, cooldown and quarantine, so a switch has to be
  worth making rather than merely momentarily better.
- **The release pipeline.** Six static targets built `CGO_ENABLED=0`, an
  enforced `sha256sums.txt`, a keyless build-provenance attestation, and both
  installers.

[Unreleased]: https://github.com/Kweiza/ccdaddy/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Kweiza/ccdaddy/compare/v0.1.0-rc1...v0.1.0
[0.1.0-rc1]: https://github.com/Kweiza/ccdaddy/releases/tag/v0.1.0-rc1
