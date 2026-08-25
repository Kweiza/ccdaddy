# Contributing

Thank you for looking. This is a small, single-maintainer project, and the
sections below are mostly about one thing: `ccdad` moves live credentials
between files, so the bar for "this works" is higher here than the size of the
codebase suggests.

## Before you write code

**Open an issue first** for anything that changes behaviour. Not as a formality
— several things that look missing are deliberate, and the README's *What is
not here yet* section says which. An issue costs you five minutes and can save
you an afternoon.

Small things need no issue: a typo, a broken link, a comment that has gone
stale, a test that passes for the wrong reason.

## Getting set up

```sh
git clone https://github.com/Kweiza/ccdaddy
cd ccdaddy
go build ./cmd/ccdad
```

Go 1.26.4 or newer — `go.mod` is the authority, and CI reads the version from
it.

There is nothing else to install. The project's third-party modules are all Go,
and it builds `CGO_ENABLED=0` on every target. `go.mod` is the authority.

**Point it away from your real accounts while you work.** `CCDAD_HOME` moves
ccdad's own store; `CLAUDE_CONFIG_DIR` and `CLAUDE_SECURESTORAGE_CONFIG_DIR`
move the files it treats as Claude Code's:

```sh
export CCDAD_HOME=$(mktemp -d)
export CLAUDE_CONFIG_DIR=$(mktemp -d)
export CLAUDE_SECURESTORAGE_CONFIG_DIR=$CLAUDE_CONFIG_DIR
go run ./cmd/ccdad doctor
```

`ccdad doctor` is the fastest way to see what a given environment resolves to,
and it never creates anything it is checking for.

## Running the checks

```sh
scripts/ci.sh all
```

That is not a convenience wrapper — it is the same script `.github/workflows/ci.yml`
calls, one subcommand per job, so a green run here and a green run there mean
the same thing:

| Subcommand | What it does |
|---|---|
| `fmt` | `gofmt` over every Go file the repository publishes |
| `vet` | `go vet ./...` for the host |
| `test` | `go test ./... -race` |
| `cgo` | all six release targets build and vet with `CGO_ENABLED=0` |
| `cites` | no comment cites a document this repository does not contain |

The only thing CI adds is running `test` on Linux, macOS and Windows instead of
one of them. That matters more than it sounds: every `_windows.go` file in this
tree is compiled by nothing else.

A pull request from a fork gets its own CI run. One from a branch in this
repository is already covered by the run its push triggered, and the workflow
skips the duplicate on purpose — `.github/workflows/ci.yml` explains why in the
comment above its triggers.

## What this project means by "tested"

A passing test is not the bar. The standing question for any test here is:

> **Which wrong implementation does this actually rule out?**

Test suites in this repository have been caught passing for the wrong reason
more than once — a comparator whose fixtures happened to sort the expected way,
two branches that returned the same exit code for opposite reasons, an
assertion on a filesystem timestamp that the kernel's clock granularity made
unfalsifiable. So:

- **Break the behaviour on purpose and watch a test fail.** Delete the guard,
  invert the condition, drop the branch — then run the package. If everything
  is still green, the test does not constrain the thing you thought it did.
  Saying "I removed X and `TestY` failed" in your pull request is the single
  most convincing line you can write.
- **When two branches produce the same level or the same exit code, assert on
  what only one of them says.** The severity is not the behaviour; the guidance
  is.
- **Make fixtures contradict the accidental ordering.** If a comparator ends in
  a tie-break, name the fixtures so alphabetical order disagrees with the
  expected order — otherwise the tie-break silently stands in for the logic
  under test.
- **Keep tests off the network and out of the real home directory.** Each
  package has an `isolate` helper that points every path at a temp directory,
  points the API client at a server that fails the test if dialled, and stubs
  the environment probes. Use it. A test that needs a real one opts in by name,
  which makes the exception visible.

## Code that touches credentials

`internal/cclink`, `internal/cclock`, `internal/switcher` and `internal/store`
can destroy somebody's login. Changes there get read closely, and it helps if
the pull request already answers:

- which locks are taken, and in which order;
- whether the file is re-read *under* the lock rather than before it;
- which top-level keys survive the write. The credential swap is a **deny-list**
  of the five account-scoped keys, deliberately, so that an unknown key added by
  a future Claude Code is preserved rather than destroyed. Adding to an
  allow-list is almost always the wrong fix;
- whether anything can now happen between taking a lock and a network call.

Nothing in this repository should ever print a token value. `ccdad doctor`
prints the *names* of credential environment variables and never their
contents; keep it that way.

## Style

- **gofmt decides formatting.** `scripts/ci.sh fmt` fails on anything else.
- **Comments explain *why*.** The code says what it does; a comment that
  repeats it earns nothing. What is worth writing down is the alternative that
  was rejected, the thing that looks like a bug and is not, the platform
  behaviour that surprised you. Several comments in this tree exist purely to
  stop the next reader from "fixing" something back.
- **Comments are self-contained.** A comment may point at something in this
  repository — a file, a function, a README section — and at a published
  standard (an RFC, the Go specification). It may not point at anything else.
  For a while these comments cited a design document by section, `§7.2` and
  `§9.3`, and named internal work items; that document is not in the tree and
  is not going to be, so every one of those references was worth nothing to the
  person actually reading the code. State the fact instead.
  `scripts/ci.sh cites` fails the build on `§`, on "the brief", and on
  "task *n*" — and on a pointing phrase ("see", "per", "refer to", "described
  in", "documented in") in front of a hyphenated name this repository does not
  contain. That last one is there because a private note was once cited by
  name rather than by section, and a bare name has no spelling for a literal
  to catch. Pointing at a file that *is* in the tree is fine and stays fine.
- **The 7-bit rule is `internal/tui` and `internal/view` only.** Those two
  packages draw a terminal page whose frame is measured in columns, and their
  golden fixtures compare raw bytes — so a character that is one column wide
  on your machine and two under `RUNEWIDTH_EASTASIAN`, or that a console on a
  code page other than 65001 cannot carry, breaks a frame rather than a
  sentence. Every glyph either package emits comes out of the `Glyphs` value
  the model was built with, and there are two sets, unicode and ascii: pick
  from them rather than typing a character into a cell. The fixtures are the
  whole enforcement. There is deliberately no linter, because the rule is not
  repository-wide, and a repository-wide one would have to be wrong about a
  hundred lines to be right about the frame: measured over string literals in
  shipped non-test Go, this tree emits 106 em dashes across 101 lines and 4
  ellipses, most of them from `internal/cli/doctor.go` and
  `internal/cli/run.go`. Those are prose, they are correct, and "fixing" one
  back to a hyphen changes nothing for the better. A comment in `internal/tui`
  once claimed the whole repository emitted no non-ASCII byte; it was wrong
  by well over a hundred characters at the time, and it is the reason this
  bullet exists.

  The one exemption inside those two packages is narrower still: a frame
  corner, a rule or a gauge cell may be drawn at two columns wide under
  `RUNEWIDTH_EASTASIAN` without breaking a measured width, because a frame is
  drawn to a width it was told rather than packed against neighbouring text.
  That exemption is closed at exactly eight characters — four corners, two
  rules, two gauge cells — and it stays closed because `PickGlyphs` falls
  back to the ASCII set for any process running with that mode on, so the
  exemption never has to widen a frame it did not already draw at that width.
- **Commit messages are imperative and explain themselves.** Look at
  `git log` — the subject says what the commit does, and the body says why,
  what was considered, and what was deliberately left alone. That body is
  frequently the only record of a decision.
- **English throughout**, in code, comments, commit messages and documentation.

## What happens to your pull request

One maintainer, so: an acknowledgement quickly, a real review when there is
time to give it properly. If a change is right in substance but not in shape,
expect a conversation rather than a rewrite — and if you would rather it were
just taken over and finished, say so and it will be.
