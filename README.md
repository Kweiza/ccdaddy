<p align="center">
  <img src="assets/ccdaddy.png" width="760"
       alt="Pixel-art wordmark reading CCDaddy (ccdad) over an amber terminal frame. Below it: four small Claude characters crowded together and, off to the right, a larger one in a hat and moustache pointing at them. The caption reads 'Hey, quota's down again? You were Yap-ping!' — the Daddy Daemon.">
</p>

# ccdaddy

**Claude Code Daemon: Always Drilling, Don't Yap.** A single static binary,
`ccdad`, that manages several Claude Code accounts and moves you to the next
one *before* a rate limit stops you.

[![ci](https://github.com/Kweiza/ccdaddy/actions/workflows/ci.yml/badge.svg)](https://github.com/Kweiza/ccdaddy/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/Kweiza/ccdaddy?sort=semver)](https://github.com/Kweiza/ccdaddy/releases)
[![license](https://img.shields.io/github/license/Kweiza/ccdaddy)](LICENSE)

```console
$ ccdad list
  IDX  ACCOUNT                  TYPE          TIER  LEFT  RESETS IN
* 1    work@example.com (work)  subscription  max   18%   1h 14m
  2    personal@example.com     subscription  pro   83%   4d 3h
  3    ci@example.org (ci)      api-key       -     ?     -

$ ccdad status
Daemon:  running  pid 48213  up 2h 6m
Active:  work@example.com (work)

  IDX  ACCOUNT                  TYPE          USED  WINDOW     RESETS IN  PACE     AGE
* 1    work@example.com (work)  subscription  82%   five_hour  1h 14m     ahead    41s
  2    personal@example.com     subscription  17%   seven_day  4d 3h      on pace  2m
  3    ci@example.org (ci)      api-key       -     -          -          -        -
```

`ccdad` is an unofficial, third-party tool. It is not affiliated with,
endorsed by, or supported by Anthropic.

## Contents

- [Why](#why)
- [Install](#install)
- [Quick start](#quick-start)
- [Commands](#commands)
- [How the switch stays safe](#how-the-switch-stays-safe)
- [Running sessions side by side](#running-sessions-side-by-side)
- [Configuration](#configuration)
- [Scripting](#scripting)
- [What is not here yet](#what-is-not-here-yet)
- [Building from source](#building-from-source)
- [Troubleshooting](#troubleshooting)

## Why

Claude Code stores one login at a time. If you have more than one account, you
either edit `~/.claude/.credentials.json` by hand — which is how people destroy
the MCP server logins that live in the same file — or you notice you have hit a
limit, log out, log in again, and lose your place.

`ccdad` keeps each account's credentials in its own store, watches how much of
each account's quota is left, and swaps the live login when the account you are
on is running out and another one is not. The swap takes Claude Code's own
locks, so a session in flight picks up the new login on its next request with
no restart.

## Install

The published installers verify a SHA-256 checksum before they will put
anything on your disk, and abort rather than warn when they cannot.

**macOS and Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/Kweiza/ccdaddy/main/install.sh | bash
```

**Windows (PowerShell 5.1 or newer)**

```powershell
irm https://raw.githubusercontent.com/Kweiza/ccdaddy/main/install.ps1 | iex
```

On an unpatched Windows PowerShell 5.1 host, TLS 1.2 is not the default and
`irm` fails before it reaches the script. Put the protocol in front of it:

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
irm https://raw.githubusercontent.com/Kweiza/ccdaddy/main/install.ps1 | iex
```

PowerShell 7 needs neither line.

### Installer options

Both installers take their options from the environment, because `curl | bash`
and `irm | iex` cannot pass arguments.

| Variable | Meaning | Default |
|---|---|---|
| `CCDAD_INSTALL_DIR` | Where the binary goes | `~/.local/bin` · `%LOCALAPPDATA%\Programs\ccdad` |
| `CCDAD_VERSION` | A released tag to pin, e.g. `v1.2.3` | the latest non-prerelease |
| `CCDAD_BASE_URL` | Download origin, for mirrors | GitHub releases |

The two differ on `PATH`, deliberately. **`install.ps1` registers it for you**:
it appends the install directory to the user `PATH` in the registry and
broadcasts the change, so a new shell has it. **`install.sh` does not touch a
shell profile** — the script itself is on stdin under `curl | bash`, so it
cannot ask permission, and a startup file guessed at is a startup file that can
be corrupted. It prints the `export PATH=…` line for you to paste instead.

### Verifying the download

Every release publishes `sha256sums.txt`, which the installers enforce, and a
keyless build-provenance attestation:

```sh
gh attestation verify ccdad-linux-amd64 --repo Kweiza/ccdaddy
```

Windows binaries are not Authenticode-signed yet, so SmartScreen will warn.
The attestation is the thing to check.

Each release also carries `LICENSE`, `NOTICE` and `THIRD-PARTY-LICENSES.txt`
as assets, hashed into the same `sha256sums.txt` and covered by the same
attestation — so a binary downloaded on its own still arrives with the notices
the modules inside it require.

### Upgrading

Re-run the same one-liner. Both installers stop the running daemon before
replacing the binary and neither restarts it: it comes back on the next command
that is allowed to auto-start one — `add`, `add-token`, `list`, `status`,
`switch` or `which`. `ccdad daemon status` is not one of them, on purpose, so a
supervisor loop cannot start what it was only asked to look at.

### Removing it

```sh
ccdad uninstall
```

Not `rm` — there is a daemon to stop and a credential directory to clear.

## Quick start

```sh
ccdad add work          # opens a browser; 'work' becomes the alias
ccdad add personal
ccdad list              # who is managed, and how much quota each has left
ccdad which             # who Claude Code is logged in as right now
ccdad switch personal   # move the live login
ccdad daemon start      # watch quota and switch automatically from now on
```

`ccdad add` does not switch to the account it just added. Pass `--activate`
if you want both.

On a headless machine, or when a token came from somewhere else:

```sh
ccdad add-token          # prompts without echoing, on a terminal
ccdad add-token -        # reads from stdin
```

With no argument and no terminal — in a script, or under `nohup` — `add-token`
is a usage error rather than a silent hang. Pass the token, or `-`.

## Commands

| Command | What it does |
|---|---|
| `ccdad add [ALIAS]` | Log in through the browser and manage the account |
| `ccdad add-token [TOKEN\|-]` | Register an `sk-ant-oat…` setup token or an `sk-ant-api…` key |
| `ccdad list` | List managed accounts and how much quota each has left |
| `ccdad which` | Show which managed account Claude Code is logged in as |
| `ccdad switch [ACCOUNT]` | Make an account the live login |
| `ccdad run <ACCOUNT> [args…]` | Start a Claude Code session as an account, without changing the live login |
| `ccdad auto` | Run the auto-switch engine, once or continuously |
| `ccdad status` | The engine dashboard: quota used, window, reset, pace — read from disk |
| `ccdad daemon start\|stop\|restart\|status\|logs` | Drive the background daemon directly |
| `ccdad config get\|set\|unset\|list\|path` | Read and write `~/.ccdad/config.toml` |
| `ccdad alias`, `move` | Give an account a handle; reorder the display |
| `ccdad disable`, `enable` | Hold an account out of automatic rotation, or return it |
| `ccdad export`, `import` | Move the account store between machines |
| `ccdad remove` | Stop managing an account and delete its stored credentials |
| `ccdad doctor` | Check the layout ccdad depends on, and the hazards around it |
| `ccdad uninstall` | Stop the daemon, delete the store, remove the binary |

Anywhere a command takes an `ACCOUNT`, it accepts a display index, an alias, an
email address, or a uuid prefix of at least eight characters. Alias, email and
uuid matching are case-insensitive, and there is no fuzzy matching: an ambiguous
reference is a usage error rather than a guess.

`ccdad --help` and `ccdad <command> --help` are the authority; every command
documents its own flags there.

### `ccdad switch`

```sh
ccdad switch work                        # by alias
ccdad switch --strategy headroom         # let the engine choose
ccdad switch --strategy headroom \
             --model sonnet              # ...for a Sonnet session
```

With no account, `--strategy` runs the same ranking and the same anti-flap
margins the daemon uses, against the same on-disk usage cache. It never polls
on its own — run the daemon, or `ccdad list --refresh`, so there is something
fresh to choose on.

`--model` names the model the session will run, and **narrows** the ranking: the
weekly caps scoped to other models stop counting against an account, so one
whose Opus week is spent can still be chosen for a Sonnet session. It only ever
raises an account's headroom. Caps that are not per-model — the five-hour and
all-model weekly windows, and any cap scoped to a *surface* rather than a model —
always count. Name a family (`opus`, `sonnet`, `haiku`, `fable`), with or
without a version; a name `ccdad` cannot place is refused rather than quietly
ignored.

## How the switch stays safe

`~/.claude/.credentials.json` holds more than your login. `mcpOAuth` — every
MCP server you have authenticated to — lives in the same file, and so do
several machine-scoped keys that Claude Code has added over time.

So the swap is a **deny-list**, not an allow-list. `ccdad` replaces the five
keys it knows are account-scoped and preserves everything else, including keys
it has never heard of. `ccdad doctor` tells you when it sees one, because a new
unknown key is how a tool like this silently starts leaking state between
accounts.

The rest of the protocol matters just as much:

- Claude Code's three lock directories are taken **in Claude Code's own order**,
  so the two programs cannot deadlock against each other.
- The file is re-read **under** the lock, never before it.
- The write is an atomic rename, so a reader sees the old file or the new one
  and never a half-written one.
- No network call ever happens while a lock is held.
- The credential path is opened `O_NOFOLLOW`: a symlink planted there is
  refused rather than followed.

## Running sessions side by side

```sh
ccdad run work                 # a session as 'work'; the live login is untouched
ccdad run work -- --model opus # everything after ACCOUNT goes to claude verbatim
```

`ccdad run` gives the session a credential home of its own containing only that
account's login — the smallest blast radius available. The cost is that MCP
logins do not come with it, because Claude Code keeps them in the same file.

`--full-profile` gives the account a whole config home instead, kept under the
ccdad store between runs, so its MCP logins and trust answers survive. It is
seeded once from your live config home — top-level files only, never project
history.

The exit status is `claude`'s, not ccdad's. A session killed by a signal
reports 128 plus the signal number, as a shell would.

## Configuration

`~/.ccdad/config.toml`, written by `ccdad config set` and readable by hand.
No credential ever goes in it — this is the file people paste into bug reports.

```console
$ ccdad config list
KEY                    VALUE     SOURCE
threshold              80        default
hysteresis_pct         10        default
headroom_ratio         2         default
cooldown               5m0s      default
recovery_hysteresis    5m0s      default
strategy               headroom  default
credit.max_auto_spend  0         default
```

`credit.max_auto_spend` defaults to `0`, and that is the point: an account
billed by credit is a **last resort**. Subscription quota is spent first,
unattended spending needs two independent opt-ins, and a switch that cannot
read the current spend fails closed rather than guessing.

Keys this version does not recognise are left alone rather than deleted, so a
file written by a newer release survives an older one. Trying to *set* an
unknown key is still an error — a typo that is quietly accepted is a setting
that does nothing.

### Environment

| Variable | Effect |
|---|---|
| `CCDAD_HOME` | ccdad's own store (default `~/.ccdad`) |
| `CLAUDE_CONFIG_DIR` | Claude Code's config root, honoured exactly as Claude Code honours it |
| `CLAUDE_SECURESTORAGE_CONFIG_DIR` | Claude Code's credential root, which it scopes independently |

## Scripting

### Exit codes

One contract across the command tree, which is what makes them worth branching
on. Two commands are deliberately outside it: `ccdad doctor` answers `0` when
nothing failed and `1` when something did — a warning is not a failure — and
`ccdad run` exits with **claude's** status, because it is a runner.

| Code | Meaning |
|---|---|
| `0` | The requested action was taken |
| `1` | Runtime failure — network, I/O, lock contention, token refresh |
| `2` | **Usage error only** — a bad flag, a bad combination, an unknown account |
| `3` | Understood, nothing to do (already on that account; daemon already stopped) |
| `4` | Blocked: wanted to act, no viable target (everything exhausted, credit gate refused, or `CLAUDE_CODE_OAUTH_TOKEN` set) |
| `5` | A negative answer to a probe (no daemon running; nothing attributable) |
| `130` | SIGINT |

`3` versus `4` is the actionability line — **alert on `4`, ignore `3`** — and
`2` is kept exclusively for usage errors so a cron job can tell a typo from a
no-op. `5` exists so `ccdad daemon status; [ $? -eq 5 ] && ccdad daemon start`
is safe: "no daemon" and "cannot determine whether there is a daemon" are
different answers, and a supervisor that conflates them respawns forever on a
filesystem where locks do not work.

A closed pipe is not an error: `ccdad list --json | head -1` exits `0`.

### `--json`

Every read command takes `--json` and prints a single object with a
`schemaVersion`. The one exception is `ccdad auto --json`, which emits **NDJSON**
— one event per line — because it is a stream.

### Stability contract

> **`idx` is a display ordinal, not a key.** It is recompacted whenever an
> account is removed. Scripts must reference accounts by `uuid` or `alias`.

This is printed by `ccdad --help` too. It is the one promise made before 1.0.

## What is not here yet

Deliberate, and listed so you can tell a gap from a bug.

- **A TUI and an MCP plugin.** Both are planned; neither is written.
- **No OS service integration.** The daemon manages itself — a detached
  process, a `flock` singleton, a pidfile, auto-started by any `ccdad` command.
  There is no launchd, systemd or Windows service unit in v1.
- **A setup token cannot be activated.** Claude Code reads one from
  `CLAUDE_CODE_OAUTH_TOKEN` only, never from the credential file, so there is
  nothing for `ccdad` to install. Export the variable and run Claude Code
  yourself. API keys *can* be activated, with `--activate`.
- **`ccdad which` does not attribute `ANTHROPIC_API_KEY`.** Claude Code gates
  that variable on an approved-suffix list and races it against `apiKeyHelper`
  and `primaryApiKey`; guessing would be worse than declining.
- **`ccdad run` refuses API-key accounts**, and `--full-profile` is the mode
  that could serve them one day.
- **A weekly cap scoped to another *surface* still counts against an account.**
  Claude Code is itself one surface, so a surface cap can be the very window
  that binds a session, and the response gives no way to tell which surface name
  is this client's own — so `ccdad` counts them all. `--model` narrows models,
  never surfaces.
- **`ccdad setup-path` does not exist.** The installers print a `PATH` line to
  paste instead.
- **Windows file modes.** `chmod` is a no-op there, so the store relies on the
  ACL inherited from `%USERPROFILE%`. Windows binaries are also unsigned.
- **The macOS Keychain is not used**, because Claude Code no longer uses it.
  `ccdad doctor` reports a *stale* keychain item, since a downgraded Claude
  Code would still read one.

## Building from source

```sh
git clone https://github.com/Kweiza/ccdaddy
cd ccdaddy
go build ./cmd/ccdad
```

Go 1.26.4 or newer. Eight third-party modules, all Go; the released binaries
are static and need no runtime.

```sh
scripts/ci.sh all
```

runs exactly what CI runs: `gofmt`, `go vet`, `go test ./... -race`, and a
`CGO_ENABLED=0` build of all six release targets.

### `go install`

```sh
go install github.com/Kweiza/ccdaddy/cmd/ccdad@latest
```

This works, with one caveat worth knowing before you rely on it: **a binary
built this way cannot be version-checked or upgraded by the installer.** The
version stamp comes from link-time flags that only the release build sets, so
`go install` falls back to the VCS revision and `ccdad --version` reports a
commit rather than a tag. Nothing can compare that to a release, so upgrades
are yours to manage.

## Troubleshooting

Start here:

```sh
ccdad doctor
```

Thirteen checks over the store, its permissions, whether file locking works on
this filesystem at all, the daemon's pidfile and status file, the usage cache,
the engine state, the config, leftover session directories, Claude Code's
credential file and its top-level keys, a stale legacy keychain item, and the
environment variables that would make a switch a no-op.

It **reports**; it repairs nothing and creates nothing it is checking for — a
diagnostic that manufactures the directory it was asked about is a diagnostic
that lies. It never prints a credential value, which is what makes
`ccdad doctor --json` safe to paste into an issue.

Common answers it gives:

| It says | It means |
|---|---|
| `warn store … does not exist` | Either ccdad has never run here, or `CCDAD_HOME` points somewhere unintended |
| `fail locks` naming NFS or CIFS | The store is on a filesystem without working locks. Move `CCDAD_HOME` onto local storage |
| `warn environment … CLAUDE_CODE_OAUTH_TOKEN` | Claude Code reads that instead of the credential file. An unattended switch is **refused** rather than made pointless — `ccdad auto` reports exit 4 |
| `warn credential-keys` | Claude Code has added a key ccdad does not know. It is preserved, not destroyed — but please open an issue |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Issues and pull requests are welcome;
open an issue first for anything that changes behaviour.

Security reports do **not** go in the issue tracker — see
[SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE), [NOTICE](NOTICE) and
[THIRD-PARTY-LICENSES.txt](THIRD-PARTY-LICENSES.txt).
