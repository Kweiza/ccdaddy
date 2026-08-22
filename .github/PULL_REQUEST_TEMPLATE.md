<!--
Thank you for this. Two things worth knowing before you fill it in:

  * Run `scripts/ci.sh all` locally. It runs exactly what CI runs — gofmt, go
    vet, `go test ./... -race`, and a CGO-free build of all six release
    targets — so a green run here and a green run there mean the same thing.
    The only difference CI adds is running the tests on three operating
    systems instead of one.

  * A pull request from a fork gets its own CI run; one from a branch in this
    repository is covered by the run its push already triggered. Either way,
    look at the run before asking for a review.

CONTRIBUTING.md has the longer version, including what this project means by
"tested".
-->

## What this changes

<!-- One or two sentences. If it fixes an issue, "Fixes #123". -->

## Why

<!--
The part that is not recoverable from the diff. What went wrong, what you
considered and rejected, which reading of an ambiguous spec you took. This
project keeps its reasoning in commit messages and code comments rather than
in a wiki, so this section usually becomes one of those.
-->

## How it was verified

<!--
Not "tests pass" — which tests, and what would break them.

This project's standing question for any new test is: *which wrong
implementation does this actually rule out?* A test that passes against the
code with the change reverted is not evidence. If you deleted the behaviour on
purpose and watched a test fail, say so; that is the most convincing line you
can put here.
-->

- [ ] `scripts/ci.sh all` is green locally
- [ ] New behaviour has a test that fails when the behaviour is removed
- [ ] Comments explain *why*, where the code cannot

## Anything touching credentials?

<!--
Delete this section if not.

ccdad reads and writes Claude Code's live credential file. Changes anywhere
near `internal/cclink`, `internal/cclock`, `internal/switcher` or
`internal/store` can destroy somebody's login, so say explicitly:

  * which locks are taken, and in which order;
  * whether the file is re-read under the lock;
  * which top-level keys survive a write, and why the deny-list is still right;
  * whether anything can now run between taking a lock and a network call.
-->
