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

- **ccdad can name the Claude Code that is installed, and does it without
  running one.** A new `internal/ccver` reads the version off the install layout
  — a native launcher is a symlink into `<data home>/claude/versions/<VERSION>`,
  so one `readlink` names it, and an npm install of any era resolves into
  `node_modules`, so `@anthropic-ai/claude-code/package.json` names it. The
  obvious source, `claude --version`, is deliberately not used: the native
  launcher resolves and can UPDATE itself when invoked, so that probe would
  change what it measures, and `ccdad doctor`'s first rule is that a probe must
  not disturb its subject. `~/.claude.json` is not a source either —
  `lastOnboardingVersion` lagged the installed release by 118 patch versions on
  the machine this was written on.
- **A Windows native install is named too, from the bytes of its launcher.**
  There is no symlink to read there: the installer copies the versions binary to
  `~/.local/bin/claude.exe`, and it records nowhere which one it took. ccdad
  identifies it by comparing that launcher's CONTENT against the binaries still
  in the versions directory — exact, and the only thing that is. Comparing
  *sizes* is what Claude Code itself does, in its Windows update and again in
  its orphan cleanup, and it is already wrong: two releases on the machine this
  was written on are byte-for-byte the same LENGTH and different builds. That
  size check is also why a Windows launcher can hold an older build than the
  newest one installed — the update skips the copy when the sizes match — so
  reading the bytes is the only way to name what actually runs. Without this,
  `ccdad run`'s refusal never fired on Windows: a user on 2.1.112 or earlier got
  a default-mode session that silently ran as the machine's live login, because
  `CLAUDE_SECURESTORAGE_CONFIG_DIR` does not exist on those builds on any
  platform. Two launchers that cannot be told apart, or a launcher matching
  nothing installed, still read as "found an install and cannot name its
  version" — a wrong version is worse than an unknown one, and it is the only
  answer that makes ccdad refuse to start on a machine that works.
- **ccdad looks for Claude Code under BOTH of the home directories it can be
  installed in.** Claude Code resolves the home under `~/.local` as
  `HOME ?? os.homedir()` and the home under `~/.claude` as a plain
  `os.homedir()`. On Unix those are one directory. On Windows they part the
  moment `HOME` is set — a Git-for-Windows shell sets it by default — so an
  install performed from PowerShell lands under `%USERPROFILE%` and one
  performed from an MSYS2 or Cygwin shell lands under `$HOME`, and Claude Code
  itself installs under the first home while searching under the second. ccdad
  searches both, and on Windows compares those paths case-insensitively, which
  is how the filesystem compares them.
- **`ccdad doctor` gains a `claude-version` check, for eighteen.** It names the
  version, how Claude Code was installed and which launcher it read, and it
  **fails** on 2.1.112 or earlier — the era where a keychain item shadows every
  switch and `ccdad run`'s default scoping is ignored, so a green report would
  be telling you the machine is fine while nothing ccdad does reaches Claude
  Code. A launcher it cannot classify is a warning, not a failure: ccdad not
  being able to read an install is not the install being broken.
- **`ccdad doctor` gains three checks — `path`, `profiles` and `api-key` — for
  seventeen.** `path` answers `ccdad: command not found` by reading two facts
  rather than one: whether the binary's directory is on the PATH of the shell
  you are in, and whether `ccdad setup-path` has registered it in a startup
  file. Those disagree in the ordinary case — a registration is written for a
  NEW shell to read — and which of the two it is decides whether the remedy is
  "open a new shell" or "run a command". It is never a failure: ccdad invoked by
  its full path works exactly as well. `profiles` reports a `ccdad run
  --full-profile` directory whose account is gone, which since profiles began
  holding an api-key account's `primaryApiKey` is a stored credential nothing
  else on the machine would mention again; it is a set difference against the
  account list, so a profile in daily use is not reported at all. `api-key`
  names which of Claude Code's five API-key sources would actually win, and
  whether that win displaces the OAuth login.

### Changed

- **`ccdad doctor`'s stale-keychain-item remedy stops asking you which Claude
  Code you are on.** The remedy inverts across 2.1.113 — after it, deleting the
  item is cleanup; before it, the item is your live login and the next token
  refresh recreates it and deletes `.credentials.json` with it — and the row
  used to print both and leave you to decide. It now reads the version and gives
  the one that applies, leading with the cost rather than the command. Only an
  install ccdad could not classify still gets both.
- **`ccdad run` refuses in its default mode on Claude Code 2.1.112 or earlier.**
  That mode scopes with `CLAUDE_SECURESTORAGE_CONFIG_DIR`, which does not occur
  even once in 2.1.112 — the variable arrived in 2.1.113, after the keychain
  backend it nominally outranks was already gone. So on such a build the
  scoping was inert: `claude` read the machine's own credentials file and the
  session ran as the LIVE login while ccdad reported success. The refusal is a
  usage error naming `--full-profile`, which scopes `CLAUDE_CONFIG_DIR` and does
  work there. It fires only on a version ccdad actually read, and only for
  accounts whose login is a credentials file: a **setup-token** account is
  scoped by `CLAUDE_CODE_OAUTH_TOKEN`, which every era reads and prefers over
  the stored login, so those sessions still run; an **API-key** account keeps
  its own, accurate refusal. An install ccdad cannot classify starts as before.

### Fixed

- **A Windows uninstall that could not schedule its own cleanup no longer
  reports that the binary could not be removed.** Removing the running binary
  is two steps on Windows and they fail separately: renaming the .exe aside —
  which is what stops `ccdad` resolving — needs no privilege, while the
  reboot-time delete that tidies the leftover writes to a machine-scoped
  registry key and needs administrator rights. The second step failing is the
  ordinary outcome for every install that did not need elevation in the first
  place, and the line printed for it began `… could not be removed`, which
  sends a user looking for a binary that is no longer at that path, or
  reinstalling over a machine that is already clean. It now says the binary is
  gone from that path and names the single leftover file, which is all that is
  actually left.

- **The legacy keychain item is derived the way the builds that WROTE one
  derived it.** ccdad was using the formula carried by today's Claude Code,
  where `CLAUDE_SECURESTORAGE_CONFIG_DIR` outranks `CLAUDE_CONFIG_DIR` and the
  account is validated against `^[a-zA-Z0-9._-]+$`. That code is dead — it has
  never written an item — and the variable does not occur even once in 2.1.112,
  the last release that read the Keychain at all. Two false "no legacy item"
  answers came out of it: inside a `ccdad run` session, which sets that variable
  by design, `doctor` hashed the session's credential directory and looked for an
  item that cannot exist; and on a machine whose username has a space or a
  non-ASCII letter in it, `doctor` looked under `claude-code-user` while the real
  item sat under the real name.

- **The legacy item is looked for under both spellings a keychain-era Claude
  Code could have written.** 2.1.38 started NFC-normalizing `CLAUDE_CONFIG_DIR`
  before hashing it into the item's name; 1.0.30 through 2.1.37 hashed the bytes
  as they came. A decomposed value therefore has an item under each digest
  depending on which build wrote it, and `doctor` probed only the composed one —
  a third false "no legacy item", from the same cause as the other two. Both are
  derived now, composed first, and they collapse to a single lookup whenever the
  variable is unset or already composed, which is every ordinary machine.

- **`ccdad doctor` says which machine the removal command is for, before it
  offers the command.** Claude Code 2.1.112 and earlier read the keychain item
  *before* `.credentials.json` and fall back to the file when it is absent, so
  deleting it does redirect those builds — but not durably. From 1.0.36 a
  successful keychain write deletes the credentials file whenever the pre-write
  keychain read was empty, which after a deletion it always is, so the next
  access-token refresh recreates the item and unlinks ccdad's file with it. On
  2.1.113 or later the removal is clean-up nothing can undo; on 2.1.112 or
  earlier it is not a fix at all and the item *is* the live login, so the answer
  there is to upgrade Claude Code. The check now names both version numbers and
  says all of this ahead of the `security` invocation.

- **`ccdad doctor` no longer reports `ok environment` while a switch is being
  defeated.** The `environment` check looked at two variables —
  `CLAUDE_CODE_OAUTH_TOKEN` and `ANTHROPIC_API_KEY` — and printed "nothing set
  that would make a switch a no-op" for a machine with `ANTHROPIC_AUTH_TOKEN` or
  `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR` set, both of which ccdad's own code
  already counted as displacing an existing login. All four are named now. The
  two remaining sources are not variables at all — the `apiKeyHelper` setting
  and the key stored in `~/.claude.json` — and the new `api-key` check covers
  them by running the resolver rather than by listing them, because the stored
  key is the one source that does NOT displace a login and `ccdad switch` writes
  it for every api-key account: a hazard list would have warned about ccdad's
  own steady state forever.

- **`ccdad daemon logs --follow` no longer goes silent at a log rotation on
  Windows, and the rotation on the other side of that race is no longer a coin
  flip.** Opening a file while a rename is in flight is where Windows answers
  `ERROR_SHARING_VIOLATION`, `ERROR_ACCESS_DENIED` or `ERROR_LOCK_VIOLATION` —
  an antivirus scanner or the search indexer holding it for a moment, measured
  at roughly 44% of replaces. None of the three is "not found", so the follower
  treated them as fatal and ENDED: someone watching the log got nothing from the
  rotation onwards, with the command already gone. It waits for the next poll
  now, and holds its position rather than guessing the file was replaced, which
  would replay the whole log for a rotation that never happened.

  The rotation itself had the same gap and a worse consequence. It closes the
  log's descriptor BEFORE renaming, and a rename that failed returned without
  reopening — so every later line went to a closed descriptor and vanished, and
  `RotateIfLarge` failed on the `Stat` it starts with, which meant the next tick
  could not recover either. One rename that lost a race cost the daemon its log
  for the rest of its life. The renames are retried now, on the same bounded
  policy `cclink` already used for credential writes, and the log is reopened
  whatever happens: a failed rotation costs one tick instead of the log. The
  errno set both sides consult is one copy, in `internal/winerr`, so they cannot
  drift.

  Three comments in the tree said Go passes `FILE_SHARE_DELETE` on `os.Open` and
  `os.OpenFile`. It does not — `syscall.Open` asks for `FILE_SHARE_READ` and
  `FILE_SHARE_WRITE` only — and that false premise is what excused both missing
  retries. They now say what the standard library does.

## [0.2.0] — 2026-08-23

Three things that were quiet in 0.1.0 answer back now, and an upgrade is where
you meet them. Bare `ccdad` was a no-op that exited 0; it is the dashboard now,
so without a terminal on both ends it is a usage error. Nine commands refuse to
run inside a `ccdad run` session rather than acting on that session's private
copy of Claude Code's state. And `switch --model`, which 0.1.0 documented as
having no effect, now narrows the ranking and rejects a model name it cannot
place. Two smaller ones: `ccdad uninstall` takes back the `PATH` entry ccdad
registered, and `ccdad doctor` gained a `credential-home` check that can fail a
machine 0.1.0 passed.

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

[Unreleased]: https://github.com/Kweiza/ccdaddy/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/Kweiza/ccdaddy/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Kweiza/ccdaddy/compare/v0.1.0-rc1...v0.1.0
[0.1.0-rc1]: https://github.com/Kweiza/ccdaddy/releases/tag/v0.1.0-rc1
