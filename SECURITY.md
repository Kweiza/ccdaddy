# Security

`ccdad` exists to move live Claude Code credentials between files. It holds
OAuth **refresh** tokens — the long-lived half — for every account it manages,
and it writes the file Claude Code authenticates with. A defect here does not
crash a program; it hands somebody else an account.

Please read the two sections below before reporting: the first is how to reach
me privately, the second lists things that look like vulnerabilities and are
documented limitations, so you can tell quickly which one you have.

## Reporting a vulnerability

**Do not open a public issue.** Use GitHub's private reporting:

<https://github.com/Kweiza/ccdaddy/security/advisories/new>

That gives us a private thread on this repository, so nothing is disclosed
until there is something to install.

What helps most, in order:

1. What an attacker gets. "Reads another account's refresh token", "makes
   Claude Code authenticate as an account the user did not choose", "leaves a
   credential world-readable" — the consequence, not the mechanism.
2. What they need first. Local unprivileged access on the same machine? A
   directory the victim writes to? A hostile `PATH`? A network position?
3. A reproduction. `ccdad doctor --json` output is safe to paste and usually
   frames the machine well; it prints paths, file modes and the *names* of
   credential environment variables, never their values.
4. The version. `ccdad --version` gives the tag and the commit.

**Never paste a real token, even a revoked one.** If a token is central to the
report, describe its shape and say which field it came from.

There is no bounty. Expect an acknowledgement within a few days; this is a
single-maintainer project, so please say so in the report if you have a
disclosure deadline and we will work to it.

## Supported versions

Pre-1.0. Only the latest release gets fixes, and there are no backports. When a
fix ships, the advisory names the first release that carries it.

## What ccdad holds, and where

Knowing this makes a report much easier to write.

| What | Where | Protection |
|---|---|---|
| Per-account credential snapshots (access + refresh tokens) | `$CCDAD_HOME/credentials/` — default `~/.ccdad/credentials/` | directory `0700`, files `0600` |
| Account metadata (uuid, email, alias, plan tier) | `$CCDAD_HOME/accounts.toml` | `0600` |
| Claude Code's live login | `~/.claude/.credentials.json` (moved by `CLAUDE_CONFIG_DIR` / `CLAUDE_SECURESTORAGE_CONFIG_DIR`) | `0600`, opened `O_NOFOLLOW` |
| Managed API keys | `~/.claude.json` → `primaryApiKey`, written by Claude Code's own rules | as Claude Code writes it |
| Per-session credential directories | under `$CCDAD_HOME`, created by `ccdad run` | `0700` |

`ccdad doctor` reports on every one of these without printing a value, which is
why it is the right thing to attach to a report.

Two things ccdad deliberately does **not** do, both of which have bitten
similar tools:

- It never resolves `security`, `claude` or any other credential-adjacent
  binary through `PATH` when an absolute path exists.
- It never overwrites credential keys it does not recognise. The credential
  swap is a **deny-list** of the five account-scoped keys, so an unknown
  machine-scoped key added by a future Claude Code is preserved rather than
  destroyed. `ccdad doctor` warns when it sees one it has not heard of.

## Known limitations that are not vulnerabilities

These are documented decisions. A report about one of them is welcome as an
*issue*, but it will not be treated as an advisory.

- **Windows file modes.** `chmod` is a no-op on Windows, so ccdad relies on the
  ACL inherited from `%USERPROFILE%`. A machine whose profile directory is
  readable by other users leaks the store. Spec §10.3; there is no v1 fix.
- **Local access reads everything.** ccdad is not a vault. Anything running as
  the user can read `~/.ccdad`, exactly as it can read `~/.claude`. The threat
  model is other *users* and other *processes' mistakes*, not the user's own
  shell.
- **The macOS Keychain is not used.** Claude Code stopped using it — its own
  secure-storage backend is named `plaintext` — so ccdad matches the file it
  actually reads. `ccdad doctor` reports a *stale* keychain item because a
  downgraded Claude Code would prefer it, and prints the `security` command
  that removes it.
- **Windows binaries are unsigned.** There is no Authenticode certificate yet
  (spec §11.4 is an open question), so SmartScreen and some antivirus products
  will warn. Verify the download instead — see below.
- **`ANTHROPIC_API_KEY` is not attributed.** `ccdad which` refuses to say which
  managed account an `ANTHROPIC_API_KEY` belongs to, because Claude Code gates
  that variable on an approved-suffix list and races it against `apiKeyHelper`
  and `primaryApiKey`. Guessing would be worse than declining.

## Verifying what you downloaded

Every release publishes `sha256sums.txt`, and both installers **refuse to
install anything they cannot verify against it** — a missing or malformed sums
file aborts rather than falling back.

Every release also carries a keyless build-provenance attestation, so you can
check that a binary came out of this repository's own workflow:

```
gh attestation verify ccdad-linux-amd64 --repo Kweiza/ccdaddy
```

If you obtained a `ccdad` binary anywhere other than this repository's releases
page, that command is the one thing worth running before you let it near your
credentials.
