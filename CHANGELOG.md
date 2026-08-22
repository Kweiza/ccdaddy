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

Everything below is on `main` and has not been published in a release yet.

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
  run.

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

[Unreleased]: https://github.com/Kweiza/ccdaddy/compare/v0.1.0-rc1...HEAD
[0.1.0-rc1]: https://github.com/Kweiza/ccdaddy/releases/tag/v0.1.0-rc1
