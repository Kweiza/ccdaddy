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

There is nothing else to install. The project has eight third-party modules,
all Go, and it builds `CGO_ENABLED=0` on every target.

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
- **Commit messages are imperative and explain themselves.** Look at
  `git log` — the subject says what the commit does, and the body says why,
  what was considered, and what was deliberately left alone. That body is
  frequently the only record of a decision.
- **English throughout**, in code, comments, commit messages and documentation.

## A thing you should know before reading the comments

The code refers to a design specification by section — `§3.3`, `§8.2`,
`§13 Q4`. **That document is not in this repository.** It is a working file
outside version control, so those references currently resolve only for the
maintainer. This is a known rough edge for outside contributors; if a section
reference is blocking you, ask in the issue and the relevant part will be
quoted or published.

## What happens to your pull request

One maintainer, so: an acknowledgement quickly, a real review when there is
time to give it properly. If a change is right in substance but not in shape,
expect a conversation rather than a rewrite — and if you would rather it were
just taken over and finished, say so and it will be.
