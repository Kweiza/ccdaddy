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
* 1    work@example.com (work)  subscription  max   18%   1h14m
  2    personal@example.com     subscription  pro   83%   4d3h
  3    ci@example.org (ci)      api-key       -     ?     -

Runway:  7d dry 2026-08-25 20:53 UTC (1d15h)  ·  5h holds  ·  basis 4h00m

$ ccdad list --json
{
  "schemaVersion": 1,
  "accounts": [
    {
      "uuid": "0d9e4e6a-1f1a-4b5e-9c3a-2f7b6a1d8e40",
      "idx": 1,
      "email": "work@example.com",
      "alias": "work",
      "kind": "subscription",
      "tier": "max",
      "active": true,
      "usage": {
        "fetchedAt": "2026-08-24T05:45:10Z",
        "ageSeconds": 41,
        "headroomPct": 18,
        "slack": -2,
        "windowThreshold": 80,
        "bindingWindow": "five_hour",
        "windows": {
          "five_hour": { "utilizationPct": 82, "resetsAt": "2026-08-24T06:59:51Z" }
        }
      }
    },
    {
      "uuid": "5b2c7f31-8a4d-4c9e-9d0a-3e6f1b2c9a71",
      "idx": 2,
      "email": "personal@example.com",
      "kind": "subscription",
      "tier": "pro",
      "active": false,
      "usage": {
        "fetchedAt": "2026-08-24T05:45:10Z",
        "ageSeconds": 41,
        "headroomPct": 83,
        "slack": 63,
        "windowThreshold": 80,
        "bindingWindow": "seven_day",
        "windows": {
          "seven_day": { "utilizationPct": 17, "resetsAt": "2026-08-28T08:45:51Z" }
        }
      }
    },
    {
      "uuid": "c1a8e2d4-6b3f-4a1e-8c5d-9f0b7e2a3c62",
      "idx": 3,
      "email": "ci@example.org",
      "alias": "ci",
      "kind": "api-key",
      "active": false
    }
  ],
  "activeUuid": "0d9e4e6a-1f1a-4b5e-9c3a-2f7b6a1d8e40",
  "forecast": {
    "basis": {
      "windowSeconds": 14400,
      "observedSeconds": 14400,
      "readings": 18,
      "accounts": 3,
      "unmeasured": 0,
      "unreadable": 0,
      "ineligible": 1
    },
    "axes": {
      "five_hour": {
        "burnPpPerHour": 8,
        "burnPpPerHourHigh": 8.5,
        "replenishPpPerHour": 40,
        "holds": true
      },
      "weekly": {
        "burnPpPerHour": 3.5,
        "burnPpPerHourHigh": 4,
        "replenishPpPerHour": 1.1904761904761905,
        "holds": false,
        "dryAt": "2026-08-25T20:53:44.285714285Z"
      }
    },
    "fleet": {
      "pointsLeft": 137,
      "pointsTotal": 200,
      "dryAt": "2026-08-25T20:53:44.285714285Z"
    }
  }
}

$ ccdad status
Daemon:  running  pid 48213  up 2h06m
Active:  work@example.com (work)
Runway:  7d dry 2026-08-25 20:53 UTC (1d15h)  ·  5h holds  ·  basis 4h00m

  IDX  ACCOUNT                  TYPE          USED  WINDOW     RESETS IN  PACE     AGE
* 1    work@example.com (work)  subscription  82%   five_hour  1h14m      ahead    41s
  2    personal@example.com     subscription  17%   seven_day  4d3h       on pace  2m
  3    ci@example.org (ci)      api-key       ?     -          -          -        ?
```

`ccdad` is an unofficial, third-party tool. It is not affiliated with,
endorsed by, or supported by Anthropic.

## Contents

- [Why](#why)
- [Install](#install)
- [Quick start](#quick-start)
- [Commands](#commands)
- [The dashboard](#the-dashboard)
- [Claude Code's own tools](#claude-codes-own-tools)
- [How the switch stays safe](#how-the-switch-stays-safe)
- [Running sessions side by side](#running-sessions-side-by-side)
- [Configuration](#configuration)
- [Containers](#containers)
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
it appends the install directory to the user `PATH` in the registry, broadcasts
the change so a new shell has it, and also updates the running session's own
`PATH` — `irm | iex` evaluates the script in your current shell, not a child
process, so `ccdad` works right there without opening a new window.
**`install.sh` does not touch a shell profile** — the script itself is on
stdin under `curl | bash`, so it
cannot ask permission, and a startup file guessed at is a startup file that can
be corrupted. It points at [`ccdad setup-path`](#ccdad-setup-path), and prints the
`export PATH=…` line underneath for the shell you are standing in.

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
that is allowed to auto-start one — bare `ccdad`, `add`, `add-token`, `list`,
`status`, `switch` or `which`. `ccdad daemon status` is not one of them, on
purpose, so a supervisor loop cannot start what it was only asked to look at.

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
| `ccdad` | The dashboard, at a terminal. In a pipe, a redirect or cron it is usage on stderr and exit `2` |
| `ccdad tui` | The same dashboard, asked for by name — and in a pipe it renders once and exits `0` |
| `ccdad add [ALIAS]` | Log in through the browser and manage the account |
| `ccdad add-token [TOKEN\|-]` | Register an `sk-ant-oat…` setup token or an `sk-ant-api…` key |
| `ccdad list` | List managed accounts and how much quota each has left |
| `ccdad which` | Show which managed account Claude Code is logged in as |
| `ccdad switch [ACCOUNT]` | Make an account the live login |
| `ccdad run <ACCOUNT> [args…]` | Start a Claude Code session as an account, without changing the live login |
| `ccdad probe <ACCOUNT>` | Spend one tiny request to start a window's clock early |
| `ccdad auto` | Run the auto-switch engine, once or continuously |
| `ccdad hover on\|off\|status` | Hand every threshold and every margin to the engine |
| `ccdad status` | The engine dashboard: quota used, window, reset, pace — read from disk |
| `ccdad runway` | How fast the accounts are spending quota, and when it runs out — measured from readings already taken |
| `ccdad daemon start\|stop\|restart\|status\|logs` | Drive the background daemon directly |
| `ccdad mcp` | Serve ccdad's tools to Claude Code over the Model Context Protocol. Claude Code starts it; you do not |
| `ccdad mcp install\|uninstall` | Register that server with Claude Code, or take it back out |
| `ccdad config get\|set\|unset\|list\|path` | Read and write `~/.ccdad/config.toml` |
| `ccdad alias`, `move` | Give an account a handle; reorder the display |
| `ccdad disable`, `enable` | Hold an account out of automatic rotation, or return it |
| `ccdad own [ACCOUNT...]` | Declare which accounts THIS machine drives — see [Running ccdad on more than one machine](#running-ccdad-on-more-than-one-machine) |
| `ccdad primary <ACCOUNT> on\|off` | Rank a credit-metered seat with the subscriptions, and let it spend unattended |
| `ccdad export`, `import` | Move the account store between machines; `--base64` writes one line for a secret store |
| `ccdad bootstrap` | Import an account document named by `CCDAD_IMPORT`; a no-op when it is unset — see [Containers](#containers) |
| `ccdad remove` | Stop managing an account and delete its stored credentials |
| `ccdad doctor` | Check the layout ccdad depends on, and the hazards around it |
| `ccdad setup-path` | Put the directory holding `ccdad` on your `PATH`, durably |
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

### `ccdad setup-path`

The answer to `ccdad: command not found` right after an install. `curl | bash`
has the installer's own script on stdin, so `install.sh` cannot ask permission
to edit a startup file, and a file it guessed at is a file it can corrupt — so
it hands the job to a command you run yourself.

```sh
ccdad setup-path            # register it
ccdad setup-path --print    # show the block, write nothing
```

It writes a marker-fenced block into the startup files your shell actually
reads, and running it twice leaves one block:

- **bash** — `~/.bashrc` *and* your login file (the first of `~/.bash_profile`,
  `~/.bash_login`, `~/.profile` that exists). Both, because a login shell reads
  only the second and a terminal-emulator shell reads only the first. It never
  *creates* `~/.bash_profile`: doing so would stop bash login shells from ever
  reading `~/.profile` again.
- **zsh** — `$ZDOTDIR/.zshrc`, else `~/.zshrc`.
- **fish** — `$XDG_CONFIG_HOME/fish/config.fish`, else `~/.config/fish/config.fish`.
- **sh, dash, ksh** — `~/.profile`.
- **csh, tcsh** — not written. The line is printed for you to add.
- **Windows** — no startup file: the install directory goes into
  `HKCU\Environment` with its value kind preserved, the change is broadcast to
  running programs, and what was added is recorded under `HKCU\Software\ccdad`
  so `ccdad uninstall` can take back that entry and only that entry. This is the
  same write `install.ps1` performs.

The block guards itself, so sourcing it twice cannot duplicate a `PATH` entry,
and it is written so that an empty `PATH` never gains an empty component — which
would put the working directory on `PATH`.

Exit `3` means nothing was written, which is either "already registered" or
"already registered, and this shell has not read the file yet". It is keyed on
what is *registered*, never on the live `$PATH`: a directory that is on `$PATH`
only because you pasted an `export` line into the shell you are standing in has
no durable registration at all, and reporting "already on PATH" there would send
you away with the next terminal still failing.

`ccdad uninstall` takes it back, and takes back only what ccdad can prove it
added: on Unix that is what lies between ccdad's markers, so a `PATH` line you
wrote yourself is never touched; on Windows there are no markers, so
`setup-path` and `install.ps1` record the directory they added under
`HKCU\Software\ccdad` and an entry with no such record is left alone and named.
That matters for a `go install` or a zip install, where the directory is one you
put on `PATH` yourself and it holds your other tools.

### `ccdad probe`

A five-hour window is anchored at *first use* and does not stretch when more is
spent against it, so a clock started early is elapsed time the account gets for
free: exhaust a window four hours in and you wait an hour, exhaust one that
started when you did and you wait five. A window with no clock running also has
no pace, no projection, and nothing for the engine to rank on, and polling does
not fix that — the endpoint reports a reset only once something has been spent.
This spends the smallest thing that counts, to start the clock.

```sh
ccdad probe work                 # wake this account's five-hour window
ccdad probe work --model opus    # wake its Opus weekly cap instead
ccdad probe --all
```

It runs `claude -p "hi" --max-turns 1` in a throwaway credential home — the same
`CLAUDE_SECURESTORAGE_CONFIG_DIR` scoping `ccdad run` uses, out of the same code
— then carries any login that turn refreshed back into the store and deletes the
session directory. **The live credentials file is never written**, which a test
pins on its bytes across a probe. `--max-turns 1` is what stops a model that
reaches for a tool from turning one word into a run of turns.

It spends your quota. That is the trade, and it is said on stderr the first time
an invocation is about to spend it — once per command, not once per account.
`probe_unknown` defaults to `true` and `hover` forces it back on; see
[Configuration](#configuration) for turning it off.

`--model` names a model *family* (`opus`, `sonnet`, and so on) and chooses the
window as well as the model: with it the turn is spent against that family's
weekly cap, without it against the five-hour window every account has. A name
carrying no family ccdad knows still wakes the five-hour window, which is the
only one such a probe could honestly promise.

Five things are refused rather than spent on. An account whose credential is a
setup token or an API key has no OAuth refresh grant, so no reading could ever
be taken for it and the quota would go nowhere. A Claude Code old enough to
predate `CLAUDE_SECURESTORAGE_CONFIG_DIR` is refused for the reason
[What is not here yet](#what-is-not-here-yet) gives: the child would run as the
machine's *live* login and spend the wrong account's quota. A shell that
already exports `ANTHROPIC_AUTH_TOKEN` or `CLAUDE_CODE_OAUTH_TOKEN` is refused
too — `ccdad run`'s own displaced-credential check, unconditionally, because
that variable is what claude actually authenticates the child with, ahead of
the scoped credentials file the probe seeds; without this the turn is spent
against whatever account the variable names while the account you asked to
probe is stamped as done. A window whose clock is already running — a reset
still in the *future* — has nothing to start; one whose reset has passed is a
clock that ran down, and that is the ordinary case rather than a refusal. That
one gets exactly one probe per rollover. A window whose probes wake nothing
backs off instead: 15m, 1h, 2h, 4h and then six hours between attempts, which is
what a probe used to cost unconditionally, so an account nothing can wake is
never tried more often than before. The verdict is taken from the window and
never from the exit code — a turn can be billed and still fail, and the two look
identical from outside — so it is the *next reading* that decides, ten minutes
on, and never the poll a minute after the probe. `--force` bypasses the last two
and never the first three. `ccdad probe --all` skips disabled accounts, since a reading for
one the engine will not switch to buys nothing; a disabled account named
explicitly is still probed, because that is a human asking.

Exit `3` when no account needed one, `1` when every probe attempted failed, and
`2` with no `claude` on `PATH`. The daemon runs the same probe on its own, and
there a missing `claude` is a warning once per daemon lifetime and an account
that keeps no reset time. The daemon also never probes the account a session is
running on: that is the one probe that duplicates work outright and the one that
could cut the session off, and `ccdad probe <ACCOUNT>` stays available to a human
who wants it now. It does not poll straight afterwards either — the probe has
already spent inference budget and the reading is not there yet, so the poll that
reads what it woke replaces this tick's poll and lands a minute later. It aims
the poll *after* that at the moment the clock it just started will run down, so
the next one begins seconds after the rollover rather than whenever the idle
cadence next happens to look. And it declines outright on an account with a
window at 100% whose overage switch is not demonstrably off, because a turn there
can be billed to credits and unattended spending takes its own two opt-ins.

### `ccdad hover`

```sh
ccdad hover on
ccdad hover status
```

Hover hands the tuning to the engine. It stops reading `threshold`,
`hysteresis_pct`, `headroom_ratio`, `cooldown`, `recovery_hysteresis`,
`preempt_lead`, `strategy`, `probe_unknown`, `credit.threshold` and every
`window_threshold` entry — and `ccdad config list` grows a `HOVER` column marking
each of them, rather than hiding the row, so a number you tuned and then stopped
seeing the effect of explains itself:

```console
$ ccdad config list
KEY                             VALUE     SOURCE   HOVER
threshold                       80        default  overriding
hysteresis_pct                  10        default  overriding
headroom_ratio                  2         default  overriding
cooldown                        5m0s      default  overriding
recovery_hysteresis             5m0s      default  overriding
preempt_lead                    6m0s      default  overriding
strategy                        headroom  default  overriding
probe_unknown                   true      default  overriding
hover                           true      file     honoured
mcp_switch_without_elicitation  false     default  honoured
credit.threshold                80        default  overriding
credit.max_auto_spend           0         default  honoured
```

One `window_threshold` entry is still read for something other than its number.
A weekly cap scoped to a key this build cannot name is ranked only because a
positive entry opted it in, and that opt-in survives hover — hover replaces the
threshold, not the decision to measure the window at all. Such a key never
appears as a row in `ccdad config list`, marked or otherwise; it gets a note on
stderr instead, because `ccdad config set` cannot name it either.

**It does not override `credit.max_auto_spend`, `primary` or `disabled`.** Fully
automatic must not quietly become fully automatic *spending*: the ceiling is one
of the two independent opt-ins unattended overage requires, and a mode cannot
supply an opt-in on your behalf. `primary` and `disabled` are facts about an
account rather than tuning.

**And its thresholds do not open the credit pool.** The question "is the free
pool finished, may the paid one be reached" is asked against the threshold *you*
configured, never against the one hover derived. Hover's figure is a pace target
computed from how far through its window each account is — with six accounts a
fortnight into a week it can sit at 31 — and reading it there would start buying
credits with two thirds of the week's subscription quota unspent, on a number you
never saw.

The threshold it picks is a pace target rather than a number. Each window gets
the share of *itself* that has already elapsed, plus one account's slice of what
is left — where *usable* means an account the engine could actually hand the work
to: not disabled, not an api-key account, carrying a usage reading, and not
currently quarantined.

```text
threshold = elapsed% of this window + 100 / usable accounts
```

**It is not capped, and a target above 100 is meaningful.** A window far enough
through its own cycle earns more than 100, which reads as *no restraint*: there
is nobody to hand the work to, so nothing is being held back. Clamping it used to
seem safe and was not — the clamp fires on whichever account is furthest through
its own window, which is exactly the account whose quota expires soonest, and
above the clamp the elapsed term is gone and the pool is ordered on raw
utilization instead of on pace. Measured on three accounts resetting one, three
and five days out, the clamp doubled how far the fleet drifted from its own pace
lines *and* cost more switches doing it. The human table still stops at `100%`
and says so in a footer; `--json` carries the real figure, because `slack` is
measured against it.

A weekly window 43% elapsed — three days into a week — with four accounts gives
68, which is 43 plus 25: an account running ahead of that pace hands the work on
while the others are behind it. One account left gives 99, because there is
nobody to hand it to, so spend what is there. A five-hour window four hours in is
already 80% elapsed, so with five accounts or fewer it lands on 99 too, which is
right — it resets within the hour anyway; with eight it is 92.

A window with no elapsed share to derive from takes a fixed 80 instead, and that
covers two cases. One is a window with no clock running, which reports no reset
at all — hover forces `probe_unknown` back on so the engine's own probe path
starts it, and `ccdad hover status` says on that row what the engine will
actually do about it and when: queued for a named time, sent, waiting for the
reading that judges it, backing off after warm-ups that woke nothing, or held
because nothing on this machine can run one. Hover queues nothing itself; the
table and the daemon read the same predicate, so it cannot promise a turn the
engine would decline. The other is a reset further out than the window is long,
which is a clock no probe can fix, so that row carries no mark. A primary credit seat has no window and no reset either, so it
is held to a fixed 95 — credits do not come back at all, and the last few points
are the ones worth keeping for a session already running.

Hover also sets its own anti-flap margins: `hysteresis_pct = 3`, no
multiplicative `headroom_ratio`, a two-minute cooldown, a five-minute recovery
hysteresis, and a pre-emption lead taken from the widest poll gap actually
observed instead of from the file. The ratio is dropped because it runs on raw
headroom while the ranking orders on slack, and the two disagree hardest exactly
where hover operates. The margin is `3` rather than the stock `10` because
hover's thresholds move: two accounts binding on windows of the same length have
thresholds that rise at the same rate, so the gap between their slacks does not
close with time at all — only burn closes it, on the very account the margin is
holding you to. A margin above the spread a real pool shows will sit on an
account with ten points left while one with thirty waits.

`ccdad hover status` prints, per account and per window, the share elapsed, the
utilization, the threshold hover computed and the slack between the last two —
every input to the formula beside its output, so the arithmetic can be checked
rather than accepted:

```console
$ ccdad hover status
Hover:   on
Pool:    3 usable accounts, so each threshold is the share of its own window that
         has elapsed, plus 100/3 points. A window far enough through its own
         cycle earns more than 100, which means no restraint -- there is nobody
         to hand the work to. This column stops at 100; --json carries the rest.

  IDX  ACCOUNT            WINDOW       ELAPSED  UTIL  THRESHOLD  SLACK
* 1    work@example.com   five_hour    80%      12%   100%       +101
* 1    work@example.com   seven_day    43%      52%   76%        +24
  2    spare@example.com  five_hour    80%      74%   100%       +39
  2    spare@example.com  seven_day    43%      31%   76%        +45
  3    seat@example.com   extra_usage  -        61%   95%        +34  (primary, metered in credits)

2 row(s) show 100% because their pace target ran past it: far enough
through their own cycle that nothing is being held back. SLACK is measured on
the real figure, so those rows do not subtract; `ccdad hover status --json`
carries it.
```

`*` marks the account Claude Code is logged in as, `-` in ELAPSED is a window
with no reset to measure a share against, and SLACK is `THRESHOLD − UTIL` — the
number the ranking actually orders on. On the rows the footer names, THRESHOLD is
the ceiling rather than the derived figure, so those two columns do not subtract;
SLACK is always the real one, because it is what the engine ordered on.

It answers `0` when hover is on and `5` when it is off, printing the table
either way — so `ccdad hover status >/dev/null || ccdad hover on` is correct,
and the numbers hover *would* choose are visible to somebody still deciding. An
omakase mode is only acceptable if you can see what it chose.

### `ccdad runway`

`ccdad status` reports levels: how much of each window is spent right now. A
level cannot tell you whether that is an hour of work away from a stop or three
days. `ccdad runway` reports the *slope* — how fast the accounts have actually
been spending — and what it implies.

The daemon was already taking these readings; now it keeps them, in
`~/.ccdad/history.json`. The rate is measured over the last four hours of them.
Nothing here fetches: it costs no request against the usage endpoint, and it
does not start a daemon to answer a question about the past.

```console
$ ccdad runway
Basis:   the last 4h00m  (3 accounts, 18 readings, 0 unreadable, 1 not in rotation)
Fleet:   137 of 200 points left on the weekly axis

  AXIS     BURN      REPLENISHES  VERDICT
  5-hour   8.0 pp/h  40.0 pp/h    holds
  7-day    3.5 pp/h  1.2 pp/h     runs dry 2026-08-25 20:53 UTC  (in 1d15h)
  Credits  ?         -            ?

  The two window rows ask whether resets give quota back faster than the fleet
  spends it. Credits do not reset: that row is a balance divided by a rate,
  with nothing coming back.

Accounts:  2 usable, 6 needed to hold at this rate  (4 more)

  IDX  ACCOUNT               WINDOW     LEFT  BURN      EMPTY
  2    personal@example.com  seven_day  83    0.5 pp/h  2026-08-25 20:28 UTC
  1    work                  seven_day  54    3.0 pp/h  2026-08-25 20:53 UTC
```

**The basis is printed above the answer, on purpose.** A four-hour rate is a
speedometer: twenty minutes of readings and four hours of them support very
different claims, and you are the one who has to weigh that. `not in rotation`
counts the accounts no switch can reach — disabled, owned by another machine,
or an API key — because their quota is not the pool's to spend and none of the
figures above covers them.

**A verdict is a simulation, not a subtraction.** `REPLENISHES` is what an axis
gives back when every account's window rolls over on time. It explains the
verdict rather than deciding it, and it is deliberately generous: it counts
accounts that are already out on the *other* axis, and windows that reported no
reset at all. The verdict comes from running the rotation forward instead — one
live login at a time, spending at the measured rate, taking each rollover as it
arrives — which is how an axis whose replenishment looks ample can still run
dry.

**`Accounts:` answers that block from the other end.** The rows above say when
the pool runs out; this says how many accounts it would take for it not to. It
is the same simulation run again with seats the fleet does not have yet
appended — never a burn rate divided by a replenishment rate — so you cannot be
told `runs dry` and `you have enough accounts` on two adjacent lines. It is
measured at the *upper* end of the band, for the same reason `holds` is: the
figure has to be one that is provably enough, and being told to buy six and
running dry on six is the failure worth being conservative about.

Four forms, and no fifth:

| The fleet | The line |
|---|---|
| Is short | `5 usable, 9 needed to hold at this rate  (4 more)` |
| Holds, with room to spare | `5 usable, 3 needed to hold at this rate  (2 to spare)` |
| Holds exactly | `5 usable, 5 needed to hold at this rate` |
| Has no basis to search from | `5 usable, ? needed  (not enough history)` |

Which axis asks for the extra seat is measured rather than assumed — the two
imply different counts, and which is larger depends on the ratio of the two
measured rates. The search stops at 256 accounts and prints `more than 256`
instead of going on: at that size the weekly axis gives back 152 points an hour,
so a fleet that appears to need more than that has a measurement problem rather
than a purchasing one.

**Rates are per axis, and the axes are never added.** A percentage point of a
five-hour window and a point of a weekly one are different quantities, so there
is a rate per row and no total. Both are percentage points per hour, `pp/h`.

**`?` is unknown and never zero.** A machine that has been recording for ten
minutes is told so, rather than handed a burn of nothing and a runway of
forever; nothing is projected from a single reading. The same rule covers the
money row — a credit spend that cannot be assembled prints `?` rather than a
figure, because every default available there would only lengthen a runway made
of money. It reads `?` above for the ordinary reason: no account in the pool is
metered in credits. It also refuses when two accounts bill in different
currencies, since those amounts do not add, and when an account with no monthly
limit is spending, since a pool with no bottom cannot be given a date. The `-`
beside it is the other verdict, and it is not the same one: paid usage reports
no renewal boundary at all, so that quantity does not exist here rather than
failing to be read.

If the accounts are on different plan tiers, a note on stderr says so. A
percentage point of a Pro window and a point of a Max window are not the same
amount of work, and every sum above adds them anyway — and the seat count is
the figure that notice matters most for, because `needed` counts accounts on
the plan the fleet already has, which is not a well-defined unit on a fleet
whose plans disagree.

`ccdad status`, `ccdad list` and the terminal dashboard carry the same
measurement as a single `Runway:` line, and print no line at all when there is
no basis for one. That line picks up `· need 9 (4 more)` when the fleet is
short and nothing when it holds: a fleet that holds has its answer in the word
`holds`, and the spare count is worth a block and not a glance.

`ccdad runway --json`, `ccdad status --json` and `ccdad list --json` publish the
identical object under `forecast`. Its `fleet` object always carries
`accountsUsable` — a count of zero is a reading — and carries `accountsNeeded`
with `accountsNeededBy` only when there was a basis to search from, absent
rather than zero when there was not. A search that reached its ceiling adds
`accountsNeededCapped`, which turns the count above it into a bound.

**`--out PATH` writes that document to a file** instead of stdout, at mode
`0600`, with only a confirmation on stderr — the spelling, the mode and the
writer `ccdad export --out` already uses. It needs `--json` as well, and says
so rather than choosing for you if you leave it off: this command has two
representations, a table for a person and a document for a program, and a
destination does not say which one you meant.

```sh
ccdad runway --json --out runway.json
```

`ccdad runway --json > runway.json` already works — the `--json` contract puts
one document on stdout and every human word on stderr — so the flag is not there
to make redirection possible. It is there for three things a redirect does not
do:

1. **The mode.** A shell redirect creates the file at your umask, typically
   `0644`. This writes `0600`.
2. **Atomicity.** A redirect truncates the target before the command runs, so a
   command that then fails leaves an empty file where a good one was. This
   renames into place or leaves the old file alone.
3. **Windows.** `>` in Windows PowerShell 5.1 — the version that ships with the
   operating system — writes UTF-16 with a byte-order mark, and the result is
   not the document.

## The dashboard

`ccdad tui` opens the interactive dashboard: the accounts, their quota, the
daemon's own state, and a key for each of the things a reader of it does next.
Bare `ccdad` opens the same thing when stdin and stdout are both a terminal.

| Key | What it does |
|---|---|
| `a` | Add an account — hands the terminal to `ccdad add` and comes back |
| `s` | Switch the live login |
| `d` | The daemon screen — `S` starts, `x` stops, `R` restarts, and the log tails |
| `c` | Change the switching strategy |
| `l` | Swap the table between what `ccdad status` shows and what `ccdad list` shows |
| `q` | Quit (`ctrl+c` too) |

`up`/`k` and `down`/`j` move, `r` reloads from disk, `esc` goes back, and `?`
opens the full key list.

Every key that changes something runs the ordinary command for it, through a
fresh command tree. It gets the same refusals, the same wording and the same
exit codes typing it would give — a switch from the dashboard inside a `ccdad
run` session is refused in that command's own words.

It never fetches. Everything on the page is read from disk, because the usage
endpoint allows roughly 28–30 requests per identity per rolling hour on a
sliding window and a dashboard that polled would let one burst saturate an
account for a full hour.

It is designed for 80×24 and gets narrower gracefully: columns drop out in a
fixed order, and below 35 columns or 3 rows it says what it needs instead of
drawing a page nobody can read. With stdout redirected it renders once and
exits 0, so `ccdad tui > page.txt` is a snapshot rather than a hang.

## Claude Code's own tools

`ccdad mcp` serves ccdad's commands to Claude Code over the Model Context
Protocol, so a session can look at your accounts and move the live login
without you leaving it. It is not a command to run by hand — Claude Code starts
it, talks to it over that process's standard input and output, and stops it
when the session ends.

```sh
ccdad mcp install              # register it, machine-wide
ccdad mcp install --scope local    # this directory only
ccdad mcp install --scope project  # ./.mcp.json, committed to the repository
ccdad mcp install --print-config   # print the entry, write nothing
ccdad mcp uninstall            # remove it again
```

**The default scope is `user`, and that is not `claude mcp add`'s default.**
Anthropic's command registers into the current project (`local`); ccdad manages
a machine's logins rather than a repository's, so machine-wide is the honest
default here. Running the installer twice is exit `3` and one entry; an entry
pointing somewhere else is rewritten and both endpoints are printed.

Fifteen tools, in four classes:

| Class | Tools | What it can do |
|---|---|---|
| read | `list`, `status`, `which`, `doctor`, `config_get` | Answers a question and changes nothing ccdad owns. Three of them may start the background daemon, and say so |
| store | `enable`, `disable`, `alias`, `move`, `primary` | Writes ccdad's own account file. Never Claude Code's login |
| credential | `switch` | Rewrites the live login. **Asks the person at the keyboard first**, through the client's own confirmation prompt, and refuses on a client that cannot ask |
| daemon | `daemon_start`, `daemon_stop`, `daemon_restart`, `daemon_status` | Drives the background process, which outlives the session that started it |

Eight ccdad verbs are deliberately **not** tools, and their absence is enforced
rather than noted — a handler registered under any of these names is refused
before it runs:

| Verb | Why not |
|---|---|
| `add`, `add-token` | Need a terminal, open a browser or read a secret from one, and block for minutes |
| `run` | Replaces the process |
| `export`, `import` | Move refresh tokens off and onto the machine through text a model can read |
| `uninstall` | Deletes the thing holding your logins |
| `setup-path` | Edits shell startup files |
| `bootstrap` | Imports a secret document, as a container entrypoint concern |

`ccdad mcp` declares no `--json` flag. Its standard output carries the protocol
and nothing else; diagnostics go to standard error, which the client treats as
server logs.

### The plugin

The same server is also packaged as a Claude Code plugin, installable through
`/plugin` from this repository's own marketplace. It is optional — the
one-liners at the top of this file remain the first-class way to install ccdad
— and it is **MCP wiring only**: it ships no skills, no agents and no hooks,
and it still needs the `ccdad` binary on your PATH. Without one the plugin
installs, reports as enabled, and only `claude mcp list` says the server failed
to connect.

Installing by both paths is safe and does not run two servers. Claude Code
de-duplicates MCP servers by **endpoint** — the command plus its arguments —
and both entries name `ccdad mcp`, so a direct registration replaces the
plugin's copy rather than running beside it.

**It does rename every tool**, and that is the part worth reading twice:

```
mcp__ccdad__switch                 # registered by `ccdad mcp install`
mcp__plugin_ccdad_ccdad__switch    # registered by the plugin
```

A permission rule, a hook matcher or an allowed-tools entry written for one
spelling silently never fires under the other — no error, no warning, no log
line. `ccdad mcp install` says so when it finds the plugin already installed,
`ccdad doctor`'s `mcp-tools` row names the spelling this machine has, and
`ccdad mcp uninstall` hands the server back to the plugin. The plugin's own
[README](plugins/README.md) carries the same warning from the other direction.

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

That default needs **Claude Code 2.1.113 or later**. It scopes with
`CLAUDE_SECURESTORAGE_CONFIG_DIR`, and that variable does not exist in 2.1.112 or
earlier — an older build would ignore it, read the machine's own credentials
file, and run the session as your live account while ccdad reported success.
ccdad reads the installed version off the launcher and refuses to start rather
than run as the wrong account, naming `--full-profile`, which scopes
`CLAUDE_CONFIG_DIR` and works on every era. `ccdad doctor` reports the same fact
as `fail claude-version`.

That refusal is only for accounts whose login is a credentials file. A
**setup-token** account is scoped by `CLAUDE_CODE_OAUTH_TOKEN` in the session's
environment instead — a variable every era of Claude Code reads, and one it
prefers over the stored login — so an old build cannot defeat that scoping and
the version refusal never reaches those accounts. Preferred over the login is not
preferred over everything, which is what the third refusal below is about.

`--full-profile` gives the account a whole config home instead, kept under the
ccdad store between runs, so its MCP logins and trust answers survive. It is
seeded once from your live config home — top-level files only, never project
history.

It is also the only mode that can run an **API-key account**. Claude Code reads
an API key from `primaryApiKey` in its global config rather than from a
credential home, and the default mode leaves that file shared with your live
session on purpose — so there is nowhere to put one without changing your
machine. A profile owns a global config of its own, and the key goes there and
nowhere else. The default mode refuses and says so.

**`ccdad run` also refuses when your own shell already carries a credential
Claude Code reads before the session's.** Claude Code reads a stored login last on the
OAuth axis, so a token, a helper, an Anthropic CLI profile or a host-injected
file already in the environment you launch from outranks the login ccdad just
installed: the session would authenticate as that credential while ccdad reported
success. It applies in **both** modes — `--full-profile` scopes a different
directory, not the environment, which is inherited either way. An
`ANTHROPIC_API_KEY` wins on a different axis that this refusal does not read, so
a key exported in your shell can still take the session; `ccdad doctor`'s
`api-key` row is what reports that one.

ccdad refuses rather than removing the offending variable. Stripping would make
the guarantee true for the sources that *are* variables and leave it false for
the ones that are not, and silently overriding something you exported on purpose
is the same harm this command exists to prevent, pointed the other way. There is
no flag to override it, because the shell already has one: the refusal names
`env -u VAR ccdad run …` where there is a variable to unset, and Claude Code's
own remedy where there is not. It names the source it found rather than listing
them, so what you read is what ccdad measured. It fires before ccdad creates the
session's credential home or a `--full-profile` profile, so a refused run leaves
nothing behind to clean up. Exit `2`, like the other two refusals.

A `CLAUDE_CODE_OAUTH_TOKEN` already exported in your shell is **not** one of
these for a setup-token account: ccdad sets that variable to the account's own
token for the session it starts, so the session runs as the account you named.
For an account whose credential is a login it is one, because Claude Code reads
that variable before any credentials file.

The exit status is `claude`'s, not ccdad's. A session killed by a signal
reports 128 plus the signal number, as a shell would.

**Inside a session, the commands that write Claude Code's own state refuse.** A
session is a whole Claude Code, and everything you — or the model — type in
there inherits the session's credential home. `ccdad switch`, `auto`, `add`,
`add-token`, `remove`, `uninstall`, `ccdad daemon start` and `ccdad daemon
restart` would act on the session's copy while reporting they had changed the
live login, so they exit `2` and name the session instead. Reads are untouched:
`list`, `which`, `status`, `doctor` and `export` answer for the shell you are
in, and `ccdad doctor` says which session that is. Run the refused ones from a
shell outside the session.

## Configuration

`~/.ccdad/config.toml`, written by `ccdad config set` and readable by hand.
No credential ever goes in it — this is the file people paste into bug reports.

```console
$ ccdad config list
KEY                             VALUE     SOURCE
threshold                       80        default
hysteresis_pct                  10        default
headroom_ratio                  2         default
cooldown                        5m0s      default
recovery_hysteresis             5m0s      default
preempt_lead                    6m0s      default
strategy                        headroom  default
probe_unknown                   true      default
hover                           false     default
mcp_switch_without_elicitation  false     default
credit.threshold                80        default
credit.max_auto_spend           0         default
```

`credit.max_auto_spend` defaults to `0`, and that is the point: an account
billed by credit is a **last resort**. Subscription quota is spent first,
unattended spending needs two independent opt-ins, and a switch that cannot
read the current spend fails closed rather than guessing. `credit.threshold` is
a different number for a different kind of account, and
[Credit-metered accounts](#credit-metered-accounts) is what it does.

`window_threshold` is a table rather than a key, and the listing above shows a
row only for the windows your file actually names. There is no default row,
because a window can be named after a model or a surface the server invented —
the legal names are not a list ccdad can print in advance. A window with no
entry of its own is measured against the top-level `threshold`. Setting one is
`ccdad config set window_threshold.seven_day 60`, and
[Per-window thresholds](#per-window-thresholds) is what it changes.

`probe_unknown` defaults to `true`, and it is the one default that **spends your
quota without being asked**. A window that has never been used reports no reset
time, so it has no pace, nothing to rank on, and no way to get one except to
spend against it — so ccdad runs a single one-turn `claude` request against such
an account and schedules a poll a minute later; [`ccdad probe`](#ccdad-probe) is
the same thing on request. Set it to `false` and the window
never gains one: an unused window still reads as nothing spent, so the account
keeps the most slack in the pool and sits at the FRONT of the ordinary ranking.
What it has no answer for is pace, the projection, and where it belongs once
every account is spent. `hover` defaults to `false`; turning it on
hands every threshold and every anti-flap margin to the engine, which derives
them from each window's own elapsed fraction instead of reading them from this
file. It takes `strategy` and `probe_unknown` with them, and it forces
`probe_unknown` back **on**: a window nothing has ever spent against reports no
elapsed share, so hover has nothing to derive a threshold from until a turn wakes
it. `credit.max_auto_spend` is the one number hover leaves alone, and
[`ccdad hover`](#ccdad-hover) is the command that turns it on and shows every
number it chose.

`mcp_switch_without_elicitation` is not an engine knob — it is the one key in
this file that governs a different surface. ccdad's MCP server refuses to
rewrite the live login without asking the person at the keyboard; on a client
that cannot carry that question, this key is how you allow it anyway. It
defaults to `false`, which refuses.
`CCDAD_MCP_SWITCH_WITHOUT_ELICITATION` in the environment of the process
running `ccdad mcp` does the same thing for one client, and it **decides**
whenever it is set — an explicit `false` there takes back what the file granted
for the machine. On a client that *can* ask, you are still asked even with the
key granted: it is a fallback, not an override. Hover honours it rather than
deriving it, because hover is a policy for the switching engine and a mode that
supplied this one would be deciding that unattended also means unconfirmed.

Keys this version does not recognise are left alone rather than deleted, so a
file written by a newer release survives an older one. Trying to *set* an
unknown key is still an error — a typo that is quietly accepted is a setting
that does nothing. Inside `window_threshold` the same rule applies one level
down: a name that is not a window ccdad could rank is refused by `config set`
with exit `2`, and a well-formed name this build does not know round-trips
through the file untouched.

### Per-window thresholds

One threshold for every window says "80% used is spent" whether the window comes
back in four hours or in six days. `[window_threshold]` gives each window its own
line:

```toml
threshold = 80              # the default for any window with no key of its own

[window_threshold]
five_hour = 85
seven_day = 60
seven_day_opus = 50
"weekly_scoped:model:Opus 4.5" = 40
```

A key inside the table has to name a window ccdad can rank: one of `five_hour`,
`seven_day`, `seven_day_oauth_apps`, `seven_day_opus`, `seven_day_sonnet`, or a
scoped weekly cap beginning `weekly_scoped:model:` or `weekly_scoped:surface:`.
Anything else is refused with exit `2` and the reason, because a threshold on a
name nothing reports is a setting that silently does nothing. `cinder_cove` is
refused too even though it is a real window: its reset time is an expiry rather
than a rollover, so it is never ranked and a threshold on it would never be
consulted.

Two rules follow from the table:

- **An account is spent when any window ccdad ranks is past its own threshold.**
  The weekly cap over 60 marks the account spent whatever the five-hour window
  says, and the five-hour cap over 85 marks it spent whatever the week says.
  Past means strictly past: sitting exactly on a threshold is not over it.
- **When a weekly cap is over, that is the one the account is reported
  against.** The `WINDOW` column, the `RESETS IN` beside it and `bindingWindow`
  in `ccdad status --json` all name it, because it is the one that will not come
  back for days: telling you to wait eight minutes for a five-hour rollover,
  when the week is gone until Friday, is the wrong answer. If more than one
  weekly cap is over, the one with the least slack is named.

**Reporting and ordering are two different questions.** The figures an account
is ranked with — its slack, the percentage left, and the threshold those came
from — are always taken from the window with the *least slack*, whichever family
it belongs to. So an account can be reported against its weekly cap and ranked
on its five-hour one in the same pass, and the window in `WINDOW` need not be the
one that decided where the account sits in the list. `ccdad status --json` and
`ccdad list --json` publish `slack` and `windowThreshold` on each account's
`usage` object — the numbers the ordering was actually made on — and `ccdad auto
--json` carries the same two on every row of its `order[]`. An account whose
reading could not be taken carries neither, exactly as it carries no
`headroomPct`: unknown is never rendered as a number.

The weekly rule is not inert everywhere, though. In the ordinary order it moves
nothing. Once every account is spent the ranking switches to recovery order
(below), and a blown weekly cap is then what an account has to *wait out* — so
an account whose five-hour window rolls over in ten minutes still ranks behind
one that is genuinely back inside the hour.

Slack is `threshold − used`, for whichever window has least. The engine orders on
it rather than on raw percent left, because with a tight weekly floor those are
different questions: an account fifteen points clear of its five-hour line is a
better target than one five points from its weekly floor, even though the second
has more quota left on paper.

**One thing outranks slack: having nothing left.** Past a threshold and *empty*
are two different facts, and ccdad keeps two words for them. Slack says whether
an account should go on spending; it cannot say whether it *can*, and under
`hover` the two come apart badly. Hover caps a derived threshold at 99, so an
account at 100% is measured against 99 and reports a slack of `-1` — the best
figure in a pool where an account with half its week unspent, but early enough to
be judged harshly, reports `-22`. Ranked on slack alone the empty account wins
and the engine hands the session to the one account that cannot serve it.

So an account with a window at 100% is filed behind every account that still has
something, in both orders, and the anti-flap margins do not hold the engine on
one: the margin runs on slack, which saturates there, so no candidate could ever
clear it. The cooldown still applies. `ccdad status` and `ccdad status --json`
report the two states separately — `exhausted` is past its threshold, `empty` has
nothing left.

The same rule reads a credit-metered seat's allowance rather than a plan window,
so an enterprise seat that has spent its credits is filed the same way.

**With no `[window_threshold]` table nothing changes.** Every window on `80`
makes slack the old headroom shifted by a constant, so the order and the
spent/not-spent verdict are identical to every release before this one. There is
a test that pins it.

**One seam worth knowing about before you tighten a window.** `hysteresis_pct`
moved onto the slack axis with the ranking. `headroom_ratio` did not — a ratio is
not shift-invariant and is undefined on a negative slack, so it still measures
raw percent left. Set `seven_day = 60` and an account sitting at 59% is one point
from that floor while still showing 41 points of raw headroom, so
`headroom_ratio` (default `2`) can refuse a switch the ranking wanted. If you
tighten a window threshold and the engine stops moving, **set `headroom_ratio` to
`1`**, which switches that margin off; `1` is the lowest value the config
accepts, because anything less would let an account with less headroom displace
the live one. `hover` is not a second answer to this, it is the same one applied
for you — it sets the ratio to `1` itself. `ccdad switch --strategy headroom
--force` overrides the hold for one switch; `--force` reaches the margins only on
the targetless grammar, so a bare `ccdad switch --force` is a usage error.

**A weekly cap ccdad cannot name.** The usage endpoint files a scoped weekly cap
under a scope key, and ccdad names two of them, `model` and `surface`. That
schema is not a closed contract, so a cap can arrive under a key this build has
never seen. ccdad keeps such a cap rather than dropping it — you can see it in
the `windows` map of `ccdad status --json` — but it does **not** rank it, because
ccdad cannot state what it caps. Writing a threshold on its name is how you say
you know: add

```toml
[window_threshold]
"weekly_scoped:region:eu" = 60
```

to `config.toml` **by hand** — quoted, because the name carries colons — and it
joins the ranking from the next reading that carries it. `ccdad config set` will
not write that line: with no reading in hand it cannot tell a scope the server
really sends from a typo, so it refuses both. `ccdad config list` names such an
entry in a note of its own, separate from the one about keys that really are
ignored, and says it is being read — the loader carries a `window_threshold`
entry whatever its name is. A window name that is simply misspelled gets the
ignored note instead, which is the honest answer: no reading ever produces it. Removing the line is how you turn it back off — a `0` is refused, not
an opt-out.

A cap ccdad cannot name **at all** — no display name, and no scope key it can
build a name from — produces no window and cannot be opted into. It is counted
instead: `ccdad status --json` carries `unnamableWeeklyCaps` on the account's
`usage` object, written only when it is not zero. Absence means zero rather than
an older ccdad. There is nothing to do about a non-zero value except know that
the account is carrying quota this build has no handle on.

### When every account is over its threshold

Nothing is left to switch *to*, and the engine does not stop. It changes the
question it is asking: instead of "who has the most room", it ranks by **who
comes back first**, inside a one-hour horizon, and by who has the most slack left
outside it. The hour is fixed and there is no key for it.

An account that is actually *empty* — some window of it at 100% — sorts behind
every account that still has something, ahead of both of those keys. The soonest
reset belongs to the window that has been running longest, which is the window
most likely to be the one that ran out, so without this the mode hands the
session to the single account that cannot serve it and the user waits out the
horizon for a switch that was available immediately.

`ccdad status` prints the mode on every run where a ranking could be made —
`headroom` and `consume-first` name themselves the same way, and the line is
absent only when nothing has ever been polled. In this mode it reads:

```console
Daemon:  running  pid 48213  up 2h06m
Active:  work@example.com (work)
Mode:    recovery  (every account is over its threshold; empty accounts last, then soonest reset inside an hour, then slack)
```

`ccdad status --json` carries the same answer as `mode`, and `ccdad auto --json`
has always emitted it on its `evaluated` events. The key is absent rather than
`headroom` when no ranking could run, so a script cannot mistake "never polled"
for "plenty of room".

The pool this is about is the accounts still in the running: the subscription
accounts plus any credit seat marked primary, minus anything a rejected refresh
token has quarantined. A failed poll is not one of those: it leaves the account
unreadable, which holds the engine OUT of this mode rather than out of the pool. Last-resort credit accounts are not in it — they are metered in
money and have no plan window, so their headroom is unknown forever and counting
them would put this mode permanently out of reach.

One account that could not be read holds the engine out of this mode. An
unreadable account is neither spent nor unspent, and treating it as spent is how
an engine parks itself permanently on one expired token.

If you have set `strategy` to `consume-first`, that is the mode you get instead,
whatever the thresholds say: it is a different question — spend perishable weekly
quota before it expires — and it is answered first. Not under `hover`, which
stops reading the key and ranks on headroom: a window close to its reset already
carries a high derived threshold, so hover puts the perishable-quota answer on
the slack axis instead of into a mode of its own.

### Switching before the limit, not after it

A switch that happens when an account reads 100% happens too late: the session is
already refused. So the engine projects.

```
horizon   = the interval ccdad is blind for  +  preempt_lead  (default 6m)
projected = used now  +  burn rate × horizon
```

The blind interval is the gap between when the current reading was **taken** and
when the scheduler means to poll again — the two stamps in the cache, not the
clock — so it is the engine's real exposure rather than a constant. If any window
that binds the model on the active account projects to 100% at or before the end
of it, and some other account still has room, the engine moves.

It reads **the window that runs out first**, which is deliberately not the window
the ranking orders on. Those are different windows whenever burn rates differ,
and they always differ: a five-hour window is thirty-three times shorter than a
weekly one, so an account whose weekly cap binds at one point of slack thirty-
eight hours out would sit unswitched while its five-hour cap cut the session in
fourteen minutes.

Where it goes is the best-ranked account that is not the live one, is not
**empty**, and is not itself projected to run out inside the same horizon —
moving from an account that stops working in five minutes to one that stops in
six buys nothing and spends the cooldown. An account whose usage poller is
sitting on a `429` is taken only when nothing cleaner is on offer: the throttle
means its reading cannot be refreshed, which is a reason to prefer a candidate
ccdad can still see, and never a reason to call it spent.

That last set of tests replaced a requirement for positive **slack**, which said
the same thing only while thresholds were numbers you typed. Under `hover` the
threshold is a pace target, an ordinary pool is negative across the board, and
the requirement meant this rule could not fire at all — silently, while accounts
still held quota.

The projection runs ahead of every margin that compares two accounts as they
stand — `hysteresis_pct` and `headroom_ratio` — because a comparison that is
about to be false is not a reason to stay. It does **not** override the cooldown,
which is the only thing bounding a switch storm; when the projection fires and
the cooldown holds it, what you are told is the cooldown. It cannot reach the
last-resort credit pool at all — a pre-emptive move walks the main ranking only,
which is the subscription accounts plus any credit seat marked primary, and it
can land on one of those like any other.

The rule corrects itself, which is why the horizon is the real poll interval
rather than a constant. Polling every 60 s gives a short horizon and the switch
lands late and close to the limit, wasting almost nothing. Polling every 1800 s —
where a `429` backoff puts it — gives a long horizon and the switch lands early.
**Polling is blocked; the session is not.**

Set `preempt_lead` to `0` and the projection is off entirely. That is a supported
answer, not a broken one: the ordinary margins still run. The exception is
`hover`, which derives its own lead from the widest poll gap it has actually
observed, held between 60 s and 10 minutes, and stops reading the key — so
turning pre-emption off means leaving hover off too.

**The danger band, and what it actually buys.** At or above 95% used on the
binding window — 95% of the endpoint's limit, not of your threshold — the account
Claude Code is logged in as is exempted from sharing its identity's budget and
polls every 180 s regardless of how many accounts that identity carries. It needs
a reading that was actually taken: a poll that failed says nothing about the
account and does not put it in the band. Read honestly:

- `/api/oauth/usage` allows roughly **28-30 requests per identity per rolling
  hour**, over a sliding window — capacity comes back only as old requests age
  out, so a burst saturates the identity for up to a full hour and waiting gives
  none of it back early.
- 180 s is 20 requests an hour, which fits inside that with headroom to spare, and
  it is a **floor no rule may argue past**. Both the per-identity division and the
  post-429 backoff can only lengthen it. One thing does move in the shorter
  direction — every interval is spread by up to a tenth either way, so an
  individual poll lands between 162 and 198 seconds. That spread is not a tuning
  knob, it is what stops daemons which paused together from coming back together:
  a laptop waking, or a fleet restarting across machines, would otherwise empty
  the shared hourly budget in a single burst. A `429` imposes a 360-second floor
  and an estimate that multiplies by 1.5 each time up to 1800 s, and the estimate
  always outruns the floor — one `429` alone earns 540 s.
- What the band buys is the **ordering**, not a faster clock. An account inside it
  would otherwise take the exhausted or candidate cadence, both 600 s, so this is
  3.3x the freshness on the one account a session can be cut off on. On a shared
  identity it also skips the divisor: three accounts on one identity would put the
  live one on 540 s, and the band holds it at 180 s while the alternates stand
  down to about thirty minutes.

The band used to poll every 60 s and shorten its own freshness gate to 30 s to
let that through. That was 60 requests an hour against an allowance of 28-30 —
twice the budget, held for as long as an account sat in the band, with no movement
requirement to end it and, on a single-account identity, no division to soften it.
It is fixed, and the shape of the fix is worth stating because it is the shape
both `cswap` and `quota-board` arrived at independently: **the sustained rate is
structure, not policy.** A rule may ask for any cadence it likes and the floor
holds it, in one place, after every other rule has had its say. A floor that only
holds as long as every author remembers it is not a floor.

The one exemption is the urgent cadence — the live account both *moving* and
within 15 points of its threshold — which still polls at 60 s. That one is
self-limiting: sustaining it for an hour would take 60 points of movement inside a
15-point band, so it is a burst of about fifteen requests and then the account
leaves the band on its own.

`ccdad list --refresh` is deliberately not shortened either — the hand-held path
serves any reading under 180 s old — so a scripted refresh cannot outrun the same
allowance on the one account where a `429` costs most.

### Running ccdad on more than one machine

**Declare which accounts each machine drives, with `ccdad own`.** This is the one
piece of multi-machine setup ccdad cannot do for you, and skipping it is the
failure it exists to prevent.

Ranking is a pure function of readings the *server* shares between your machines,
and every comparator ends in the same tie-break. Two installs given the same pool
therefore pick the same target at the same moment: both sessions land on one
account, burn its five-hour window twice as fast, and hit a rate limit while the
rest of the pool sits idle. Nothing detects it at runtime — every lock ccdad holds
is a file lock on one machine, and the same is true of both projects it was
written against.

```console
$ # on the laptop
$ ccdad own work@example.com personal@example.com
This machine drives: personal@example.com, work@example.com
Another machine drives: ci@example.org, spare@example.com

$ # on the desktop, the other half
$ ccdad own ci@example.org spare@example.com
```

An account this machine does not own is neither rotated into nor polled on a
cadence, and **an account added later belongs to another machine by default** —
declaring a split once is meant to stay declared. Two things still work by name,
because naming an account by hand says what you want more clearly than the split
does: `ccdad switch` activates one, and `ccdad list --refresh` reads one.

The live account is always polled even when another machine owns it. ccdad's
thresholds, its anti-flap hysteresis and its pre-emptive switch are all statements
about the account Claude Code is logged in as, and a machine blind to its own live
login has no baseline to make them from.

Run `ccdad own` with no arguments to see the current split, and `ccdad own
--clear` to give every account back to this machine.

### Credit-metered accounts

By default an account billed in credits is a **last resort**: it is kept out of
the main ranking and ordered in a pool of its own, by how much spend the ceiling
arms rather than by headroom, and the engine reaches that pool only once every
account in the main pool is known to be spent. Reaching one then needs two
independent opt-ins — the account's own extra-usage setting, and
`credit.max_auto_spend` raised above `0`. That is right when credits are overage
on top of a subscription: quota already paid for should be spent first.

It is wrong for an enterprise seat that is metered in credits and nothing else.
There is no subscription quota to prefer, and a gate that defaults to `0` means
the account can never be used at all.

```sh
ccdad primary work on
```

marks that account as one. A primary account is ranked alongside the subscription
accounts on `credit.threshold − extra_usage.utilization`, and
`credit.max_auto_spend` no longer gates it — **the flag is the opt-in**, typed by
a human. Turning it on prints what it costs before it writes, so someone who
typed it by mistake reads it while the flag is still off; turning it off writes
without a notice, because there is nothing to warn about. Its money figures are
not consulted on this path at all: `monthly_limit` and `used_credits` stay the
last-resort pool's axis.

A primary account is metered on credits rather than on a plan window, so it
reports no reset time and never has a recovery to rank on. In recovery mode that
puts it behind every account known to come back inside the hour; against the ones
that come back later it is ranked on slack like any other — unless its credits
are gone, which files it behind everything that has any. A credit utilization
that could not be read is unknown — not spent, not empty — and because a primary
seat is in the main pool, one it cannot read keeps the last-resort credit pool
closed for everyone.

`ccdad list` shows the flag as a suffix, `ccdad list --json`, `ccdad status
--json` and `ccdad which --json` carry `primary` on the account object, `ccdad
export` carries it too so it survives a move between machines, and `ccdad doctor`
names every account holding it — because "this account can spend money
unattended" is not a fact that should live only in a file.

`ccdad list --json` and `ccdad status --json` also carry a `usage.credit`
object on any account whose latest reading had overage switched on — primary
or not, since the axis is what the wire reported rather than something only a
primary account can have:

```json
{
  "credit": {
    "state": "enabled",
    "currency": "USD",
    "monthlyLimit": 100,
    "usedCredits": 25.5,
    "utilizationPct": 25.5
  }
}
```

`monthlyLimit` and `usedCredits` are already converted to the currency's major
unit — the one `max_auto_spend` is written in — and either is absent rather than
`0` when the wire did not report it: an unreported cap is not a cap of zero,
and an unreadable spend is not a spend of zero. `state` is `enabled`,
`disabled`, `blocked`, or `unknown`; `disabledReason` is added when an
organization refused overage and named why.

The human `ccdad list` table has nowhere to put those figures on a credit-only
account — it carries no five-hour or seven-day window, so there is no headroom
for LEFT to report — and the column used to read `?` for the whole class.
It now falls back to the same `usage.credit` reading: with both money figures
on the wire it prints `used/limit`, e.g. `25.50/100.00 used, 74.50 left
(USD)`; with only `usedCredits` it prints what was spent and says the account
sets no limit of its own; `?` is still what an account that failed to poll at
all shows.

### Environment

| Variable | Effect |
|---|---|
| `CCDAD_HOME` | ccdad's own store (default `~/.ccdad`) |
| `CLAUDE_CONFIG_DIR` | Claude Code's config root, honoured exactly as Claude Code honours it |
| `CLAUDE_SECURESTORAGE_CONFIG_DIR` | Claude Code's credential root, which it scopes independently |

These are two independent axes, and setting only the first is the trap.
`CCDAD_HOME` moves ccdad's own state; it does **not** move the Claude Code login
ccdad manages, which stays wherever `CLAUDE_CONFIG_DIR` (or
`CLAUDE_SECURESTORAGE_CONFIG_DIR`) points. Two shells with different
`CCDAD_HOME` values and the same credential root therefore run two engines over
one login, and they undo each other's switches — nothing is corrupted, the
account simply keeps changing back. ccdad refuses the second engine and
`ccdad doctor` names the state, but the fix is to give each store its own
`CLAUDE_CONFIG_DIR`.

## Containers

There is no published image. The `Dockerfile` at the root of this repository is a
reference: build it from a binary you built.

```sh
CGO_ENABLED=0 GOOS=linux go build -o ccdad ./cmd/ccdad
docker build -t ccdaddy .
```

It carries node, `@anthropic-ai/claude-code` and `ccdad`, so a session runs
inside it with no further setup.

### `add-token` is not enough

Worth knowing before you build a provisioning script around it. A `sk-ant-oat…`
setup token and an `sk-ant-api…` key are both stored **without** a
`claudeAiOauth` record — there is no refresh grant behind either — so the daemon
skips the account on every poll and nothing ever produces a reading to rank it
on. Such an account is stored, and it is usable as a credential, and it can
**never be ranked**. A container provisioned that way has no auto-switching at
all, which is the entire product, and nothing in `ccdad list` says so.

What carries a rankable account is `ccdad export --full`. `ccdad bootstrap`
reads one from `CCDAD_IMPORT` — a path, or `-` for stdin — and the entrypoint
runs it before it starts the engine.

### That document holds refresh tokens

**Never put it in an environment variable inline.** `-e CCDAD_IMPORT="$(cat
backup.json)"` is visible in `docker inspect`, and in `/proc/<pid>/environ` to
anything else in the namespace. `ccdad bootstrap` refuses a `CCDAD_IMPORT` that
holds a document instead of a path, and never prints the value back either way —
its output is a container log. Mount it read-only and point the variable at the
path:

```sh
ccdad export --full --out backup.json
docker run -d --name ccdad \
  -v ccdad-data:/data \
  -v "$PWD/backup.json:/run/secrets/ccdad-export:ro" \
  -e CCDAD_IMPORT=/run/secrets/ccdad-export \
  ccdaddy ccdad daemon logs --follow
```

or pipe it through `-`, and the container never has it on disk at all:

```sh
ccdad export --full | docker run -i --rm \
  -v ccdad-data:/data -e CCDAD_IMPORT=- ccdaddy ccdad list
```

`ccdad bootstrap` is idempotent, so running it on every start is the intended
use: an account already there at that uuid is updated, one that is not is added,
and an account's age is not moved by a re-run. A credential refreshed inside the
container is **not** overwritten by the older one in the document — pass
`--force` if that is what you want. It exits `0` when there was nothing to do and
when `CCDAD_IMPORT` is not set at all, and it prints nothing out of the document,
including when it refuses one: run `ccdad import` against the file from a shell
to find out what is wrong with it.

### A secret store carries one line, so `--base64` writes one

A GitHub Actions secret, a `.env` entry and most CI secret stores hold a single
string. A JSON document pasted into one arrives with its newlines intact and
breaks the file it landed in. `--base64` writes the same document as one
unwrapped line:

```sh
ccdad export --full --base64 --out export.b64   # 0600, one line, no wrapping
gh secret set CCDAD_EXPORT < export.b64
```

`import` and `bootstrap` read either form and are not told which — they sniff
it, because a ccdad export is a JSON object and so begins with `{`, which is in
neither base64 alphabet. Whitespace inside the blob is ignored, so a document
that went through `base64` without `-w0` and came back wrapped at 76 columns
still imports, and the url-safe alphabet and missing padding are both accepted:

```yaml
- run: echo "${{ secrets.CCDAD_EXPORT }}" | ccdad bootstrap
  env:
    CCDAD_IMPORT: "-"
```

**`--base64` is an encoding, not encryption.** The blob is exactly as much of a
secret as the JSON it holds, which is why `--out` still writes it `0600`, why
`--full --base64` to a terminal is still refused, and why `CCDAD_IMPORT` still
takes a path or `-` and never the document itself — a one-line document is
temptingly easy to paste into the variable, so `bootstrap` recognizes one there
and refuses it by name rather than by echoing it.

### `/data` must be a volume

Without one, every restart loses the account store, the usage cache and the
anti-flap state, and re-imports from the secret. The cache is the expensive half:
the usage endpoint allows roughly 28-30 requests per identity per rolling hour,
so a container that starts cold spends that budget again from zero.

### Both path variables are set, on purpose

`CCDAD_HOME` and `CLAUDE_CONFIG_DIR` are independent axes — see
[Environment](#environment) — and the image sets both. Setting only the first
would move ccdad's store onto the volume and leave the Claude Code login inside
the image layer, so two containers sharing one volume would run two engines over
one login and undo each other's switches.

### What the entrypoint does

```sh
ccdad bootstrap                 # a no-op unless CCDAD_IMPORT is set
ccdad daemon start || {         # 3 means one is already running
	status=$?
	[ "$status" -eq 3 ] || exit "$status"
}
exec "$@"
```

Exit `3` is tolerated by number rather than with `|| true`, and the status is
captured and re-raised rather than written as the shorter
`ccdad daemon start || [ "$?" -eq 3 ]`: under `set -e` that form exits with the
status of the failed `[`, which is `1`, so a `4` — another store's engine is
already driving this login — would reach a restart policy as an ordinary crash.
`1` and `4` both mean the container would come up with no engine behind it, and
neither is tolerated.

The command is `exec`'d, so the container's exit status is its own. Do not make
that command `ccdad auto`: the daemon already holds the engine singleton, and
the continuous form of `auto` answers `3` when it does. `sh` is the default, and
`ccdad daemon logs --follow` is the long-running command to give it when you
want the container to stay up on its own.

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
| `2` | **Usage errors, and `ccdad run`'s refusals** — a bad flag, a bad combination, an unknown account, or a session `ccdad run` will not start because it would authenticate as something other than the account you named. `ccdad auto` and `ccdad switch` report that same displaced credential as `4`, because for them it is a blocked action rather than a command that cannot be run |
| `3` | Understood, nothing to do (already on that account; daemon already stopped) |
| `4` | Blocked: wanted to act, no viable target (everything exhausted, credit gate refused, or another OAuth source outranks the credentials file) |
| `5` | A negative answer to a question, not a failure to answer it — no daemon running, nothing attributable, hover off when `ccdad hover status` asked. It has nothing to do with the `ccdad probe` command, which reports under the codes above like any other action |
| `130` | SIGINT |

`3` versus `4` is the actionability line — **alert on `4`, ignore `3`** — and
`2` is kept exclusively for usage errors so a cron job can tell a typo from a
no-op. `5` exists so `ccdad daemon status; [ $? -eq 5 ] && ccdad daemon start`
and `ccdad hover status >/dev/null || ccdad hover on` are both safe: "no
daemon" and "cannot determine whether there is a daemon" are different
answers, and a supervisor that conflates them respawns forever on a filesystem
where locks do not work.

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

- **The MCP server has no verb to start it.** `internal/mcpsrv` exists and
  serves six read tools — `list`, `status`, `which`, `doctor`, `config_get` and
  `runway` — alongside the account mutators and the daemon controls, but nothing
  on the command line launches it, so it is reachable only from its own tests. The terminal dashboard, which used to
  be listed here beside it, is written: run `ccdad` with no arguments, or `ccdad
  tui`.
- **No OS service integration.** The daemon manages itself — a detached
  process, a `flock` singleton, a pidfile, auto-started by any `ccdad` command.
  There is no launchd, systemd or Windows service unit in v1.
- **A setup token cannot be activated.** Claude Code reads one from
  `CLAUDE_CODE_OAUTH_TOKEN` only, never from the credential file, so there is
  nothing for `ccdad switch` to install. `ccdad run ACCOUNT` does set the
  variable for the session it starts, so that path works today; it is the live
  login that a setup token cannot become. API keys *can* be activated, with
  `--activate`.
- **`ccdad which` does not attribute `ANTHROPIC_API_KEY`.** Claude Code gates
  that variable on an approved-suffix list and races it against `apiKeyHelper`
  and `primaryApiKey`; guessing would be worse than declining.
- **A weekly cap scoped to another *surface* still counts against an account.**
  Claude Code is itself one surface, so a surface cap can be the very window
  that binds a session, and the response gives no way to tell which surface name
  is this client's own — so `ccdad` counts them all. `--model` narrows models,
  never surfaces.
- **Windows file modes.** `chmod` is a no-op there, so the store relies on the
  ACL inherited from `%USERPROFILE%`. Windows binaries are also unsigned.
- **`ccdad run` launches past npm's `claude.cmd`.** If `claude` on your PATH is
  npm's batch shim, ccdad reads it and runs the interpreter it names — `node
  cli.js` — directly, for every invocation rather than only the ones carrying an
  argument `cmd.exe` would eat. That takes `cmd.exe` out of the launch, so the
  arguments Windows hands your session are the ones you typed. A shim ccdad does
  not recognise still runs through `cmd.exe` as before, and there an argument
  containing `& | < > ^ % "` is refused rather than mangled.
- **The macOS Keychain is not used**, because Claude Code no longer uses it.
  `ccdad doctor` reports a *stale* keychain item, since a downgraded Claude
  Code would still read one — and names which remedy applies, because on
  2.1.112 or earlier that item is your live login and deleting it undoes itself.
- **Claude Code 2.1.112 and earlier are not supported.** That is the last
  release whose credential store reads the macOS Keychain before
  `.credentials.json`, and the last that does not know
  `CLAUDE_SECURESTORAGE_CONFIG_DIR` — so a switch can be silently shadowed and
  `ccdad run`'s default scoping does nothing. `ccdad doctor` fails on such a
  machine and `ccdad run` refuses; `--full-profile` still works.

## Building from source

```sh
git clone https://github.com/Kweiza/ccdaddy
cd ccdaddy
go build ./cmd/ccdad
```

Go 1.26.4 or newer. The third-party modules are all Go and `go.mod` is the
authority on which; the released binaries are static and need no runtime.

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

Twenty-two checks over the store, whether this binary is on your `PATH`, the
store's permissions, whether file locking works on this filesystem at all, the
daemon's pidfile and status file, the usage cache, the engine state, the config,
leftover session directories, whether the account list itself still exists,
`--full-profile` profiles whose account is gone, the accounts marked primary,
stored credential files no account names, whether a second ccdad store is
driving the same Claude Code login, which Claude Code is installed and whether
ccdad's model fits it, Claude Code's credential file and its top-level keys, a
stale legacy keychain item, the environment variables that would make a switch
a no-op, which API key Claude Code would actually use, and which OAuth source
it would take a session's credential from.

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
| `warn path … is not on PATH` | `ccdad` only works by its full path. Run `ccdad setup-path`. If it says the entry is *registered*, the block is already written and you just need a new shell |
| `warn api-key … makes it ignore the credentials file` | An `apiKeyHelper`, `ANTHROPIC_API_KEY` or a host-injected key (the descriptor variable, or `/home/claude/.claude/remote/.api_key`) wins over the login, so a switch writes a file nothing reads. The stored `~/.claude.json` key is **not** this — it does not displace a login, and ccdad writes it for every api-key account |
| `warn profiles … belong to no account` | A `ccdad run --full-profile` directory outlived its account and may still hold that account's API key. `ccdad remove` no longer leaves these |
| `fail claude-version` naming 2.1.112 | Claude Code predates the release ccdad is built against. A switch can be shadowed by a keychain item and `ccdad run`'s default scoping is ignored. Upgrade to 2.1.113 or later; `--full-profile` works meanwhile |
| `warn claude-version … cannot name its version` | ccdad found a `claude` launcher in a layout it does not recognise, so it cannot tell which era you are on. Nothing is broken; the keychain remedy just stays two-sided |
| `warn oauth-source … /home/claude/.claude/remote/.oauth_token` | A session host injected a token at a path compiled into Claude Code. It outranks the login, `ccdad run` does not scope around it, and there is no variable to unset — the fix is on the host session, not here |
| `warn oauth-source … does not carry user:inference` | The credentials file holds a login object Claude Code will not authenticate with. Sign in again |
| `warn credential-keys` | Claude Code has added a key ccdad does not know. It is preserved, not destroyed — but please open an issue |
| `warn credential-home` naming another store | Two `CCDAD_HOME` stores are driving one Claude Code login, and they undo each other's switches. Give one of them its own `CLAUDE_CONFIG_DIR`, or stop its engine |
| `fail credential-home` naming NFS or CIFS | Claude Code's credential home is on a filesystem without working locks, so ccdad cannot tell whether a second store is driving this login. The engine keeps running, unguarded |
| `warn credential-home … the running daemon is driving` | The daemon is writing a different credential home from the one this shell resolves, so its switches change a login nothing here reads. It was started from a shell that resolved a different home — `CLAUDE_SECURESTORAGE_CONFIG_DIR` decides that when it is defined, `CLAUDE_CONFIG_DIR` otherwise. Restart it from the shell whose configuration you want it to serve. Inside a `ccdad run` session the two differ by design, and the row says so rather than telling you to restart anything |
| `warn credential-files … belong to no account` | A file under the store's `credentials/` holds a live refresh token that `accounts.toml` does not name, so `list`, `remove` and the account rows above cannot see it. The path is in the message. Delete it once you have looked — `doctor` never will |
| `fail accounts-file … is GONE` | `accounts.toml` itself is missing while credential files still sit beside it — ccdad's whole account list is gone, not just one account. **Do not delete those files**; each is a login you can still recover. Restore the document from a backup, `ccdad import` an export, or run `ccdad add` once per account |
| `skipped profiles/primary-accounts/credential-files … cannot be trusted` | The `accounts-file` row above already failed, so these three have no account list to check against — read that row instead of this one |
| `ok mcp-tools` | Which spelling this machine's ccdad MCP tools have — `mcp__ccdad__*` from `ccdad mcp install`, `mcp__plugin_ccdad_ccdad__*` from the plugin. A rule written for one never fires under the other. `CLAUDE_PLUGIN_ROOT` decides the answer when ccdad is itself running as the plugin's server; in a shell the row reads Claude Code's plugin registry instead |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Issues and pull requests are welcome;
open an issue first for anything that changes behaviour.

Security reports do **not** go in the issue tracker — see
[SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE), [NOTICE](NOTICE) and
[THIRD-PARTY-LICENSES.txt](THIRD-PARTY-LICENSES.txt).
