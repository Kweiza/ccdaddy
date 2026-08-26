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
| `cites` | no line cites a document this repository does not contain |

It exits **0** when everything asked for passed, **1** when a check ran and
found a problem, and **2** only when you named a check it does not have. No
other code escapes, whatever the tool underneath a check exited with — `gofmt`
answers 2 for a file it cannot parse and `git` answers 128, and both used to
come straight out of the script, so a real finding arrived wearing the code
that means "you typed the check name wrong".

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

## Test code you have written but not run

Work here is often written out before it is made — a plan, a checklist, a pull
request that quotes the test it is about to add. Reading that back before you
run it is worth the hour, and it fails in one particular way: the reader
compiles the implementation in their head and takes the *test* as prose.

That has cost this tree twice. One change shipped seven defects, six of them in
test code and four of those the same shape — a case that could not reach the
check it was named for, because an earlier guard caught the fixture and
returned the same error the case was asserting on. A later change was read the
other way, every test block treated as code against the tree it would land in,
and that turned up eleven things worth changing before a line of it ran. Four
of the eleven would have produced a green run that proved nothing.

So read a test block you have not run as if it were about to be compiled:

- **Does the mutation actually reach the assertion?** Trace it through the
  control flow of the tree it will land in, not through the sentence that names
  it. One mutation was written against a constant that occurs zero times in the
  function it named — the branch it meant lives in a different function
  entirely. A `sed` for a string that is not there matches nothing and exits
  `0`, so the green run that follows reads exactly like a caught mutation.
- **Does the block compile?** Helper arity, redeclaration against the whole
  package — every other `_test.go` file in it counts — and imports in **both**
  directions, because an unused one is as fatal as a missing one. Three
  separate blocks failed this: one declared a helper the package already had
  under that name, one called `errors.New` in a file nothing had added `errors`
  to, and one called a constructor with three arguments where it takes five.
- **Would the assertion pass if the code under test never ran?** An assertion
  the zero value satisfies constrains nothing. One test's three assertions were
  all true of a freshly constructed value, and it threw away the one counter
  that would have said whether the thing under test was dispatched at all.
  Assert the counter first, then the shape.
- **Does the mutation break the build instead of the test?** "Delete this line"
  is a fine instruction until the line is an import's only use. The package
  then fails to compile with *imported and not used*, which reads as a caught
  mutation to nobody and as a broken tree to everybody. Assign `nil` rather
  than deleting the assignment.
- **Can each step's proof pass where the step stands?** One step's own
  verification needed a value that the step after it was going to publish, so
  it could not pass in place — and the two assertions it leaned on were vacuous
  until it did. Order the work by what each proof needs, not by subject.

Then check the reading itself. Of the forty-three findings that second pass
produced, three did not survive an adversarial re-read, one of them a confident
"this test is blind" whose experiment had been run at the wrong scope.
Re-prove every correction by mutating the real tree before you write it down: a
review nobody reviews adds work rather than removing it.

The same goes for a number. The comment above the citation check's exclusion
list in `scripts/ci.sh` says, as a measurement, how many lines each excluded
file would fail on if the check could read it. Both counts were written into
the commit that widened that check from five filename patterns to everything
git tracks, and neither was re-run afterwards. One had been a true count of the
tree before that commit and was stale by the time the commit carrying it
landed; the other counted fixtures where the check counts lines, and was never
right. They are corrected. A measurement in a comment is a claim like any
other, and the commit that widens a check is exactly where every count
describing it goes stale.

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
- **What this repository ships is self-contained.** A line may point at
  something in this repository — a file, a function, a README section — and at
  a section of a published standard. It may not point at anything else.
  For a while these comments cited a design document by section, `§7.2` and
  `§9.3`, and named internal work items; that document is not in the tree and
  is not going to be, so every one of those references was worth nothing to the
  person actually reading the code. State the fact instead.

  **Every line, not only comments**, and that is deliberate rather than an
  accident of how the check is written. A user-visible error string that cites
  `§7.2` of a document nobody has is worse than a comment that does, not
  better, and prose in `README.md` is read by more people than either.

  `scripts/ci.sh cites` fails the build on `§`, on "the brief", and on
  "task *n*" — and on a pointing phrase ("see", "per", "refer to", "described
  in", "documented in") in front of either a hyphenated name or a document
  path this repository does not contain. It searches every file git tracks,
  plus the ones you have written and not yet staged, minus anything
  `.gitignore` covers and anything git treats as binary. Three files are
  exempt because they have to quote what they forbid: `scripts/ci.sh`,
  `scripts/ci_sh_test.go` and this one.

  The name form is there because a private note was once cited by name rather
  than by section, and a bare name has no spelling for a literal to catch. It
  wants **at least three segments** — `see keychain-notes-here`, not
  `see keychain-notes`. That floor is a measurement, not taste: at two
  segments the same shape reports `per rate-limit window` and six other
  correct lines of this tree, because ordinary English behind "per" is
  indistinguishable from a document name by shape alone. A two-segment name is
  therefore a miss this gate accepts knowingly.

  The path form is there because that is how a plan is actually cited —
  `see docs/plans/2026-08-25-a-thing.md` — and the name pattern cannot see it:
  the slash ends the token before the pattern has matched anything. `docs/` is
  not in this repository and never has been, so a reference to one resolves for
  exactly the person whose machine it is on. Pointing at a file that *is* in
  the tree is fine and stays fine, in either form — a bare name resolves
  wherever the file sits, and a path is read from the repository root, so write
  `.github/SECURITY.md` or `SECURITY.md` rather than `./SECURITY.md`.

  **Citing a standard by section is fine, and the spelling is not fussy.**
  `RFC 6749 §6`, `RFC 6749, §6`, `§6 of RFC 6749`, `RFC6749 §6` and
  `RFC 6749 §§6 and 7` all pass. The section has to start with a digit:
  `RFC 9999 § whatever you like` is not a citation, it is a way of smuggling a
  `§` past the gate. The exemption is subtracted from a line rather than used
  to excuse it, so a line carrying both a real citation and a real violation
  still fails on the violation — writing `RFC 8252 §7.3` next to `task 47` does
  not launder the second. The Go specification has no exemption yet, because
  nothing here cites it and its sections are named rather than numbered; write
  one when the first citation to it is written.

  **A comment that names a test must name one that exists**, and that half is
  checked by `scripts/cited_test_names_test.go` rather than by `cites`. It is a
  separate check because it needs a parser: `cites` deliberately does not know
  what a comment is, and knowing what a *test declaration* is on top of that
  would mean teaching a shell script Go. It reads Go comments, Go string
  literals — `-test.run=` in a re-executed helper is a citation too, and a
  rename there leaves a subprocess that runs no test and exits 0 — and the
  shell scripts, which cite tests in their own comments.

  Two rules follow from how this goes wrong rather than from taste. **Write the
  name on one line**, even when it is long: the two that were found wrong had
  been broken across a comment line wrap, so the name had never existed as
  contiguous text anywhere and no grep, `git log -S` over every ref included,
  could have found either. And **a name after `-run` is checked as the pattern
  it is** — `go test ./internal/tui -run TestThePage -update` selects two tests
  on purpose — but it must still select something, because a `-run` matching
  nothing exits 0 having run nothing.

  Markdown is outside this one, deliberately: a name in a released section of
  `CHANGELOG.md` is frozen by another check, and this file needs to be able to
  write a placeholder in a sentence about how to describe a mutation.
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
