# AGENTS.md

Guidance for any agent working in this repository — Claude Code, Codex, or
otherwise. `CLAUDE.md` imports this file; there is one copy and it is this one.

## Hard constraints

These are not style preferences. Violating any of them is a defect.

- **Work in a git worktree. Always, and BEFORE the first file is modified.**

  Do it at the start of any session that will change a file — before the first
  edit, not after the first few. Read-only exploration needs no worktree; the
  moment the answer is "I am going to change something", the worktree comes
  first.

  In Claude Code that is the `EnterWorktree` tool. Anywhere else it is
  `git worktree add .claude/worktrees/<name> -b <branch>` followed by working in
  that directory — the tool only wraps those two steps and the isolation is the
  point, not the wrapper.

  This is not tidiness. Several sessions run against this repository at once and
  they share one checkout, so a branch can be swapped under a session that is
  midway through a change. It has happened: a session landed ten commits of one
  feature on top of an unrelated feature branch, because another session checked
  that branch out in the shared worktree between two of its commits. Nothing
  warns you — `git status` looks normal, the tests pass, and the branch name is
  only visible if you go and ask for it.

  Two smaller failures come from the same root and are worth knowing by name.
  `git checkout -- <file>`, used to undo a mutation-test edit, discards
  uncommitted implementation in that file as well; and a background agent that
  runs `git checkout` in the shared checkout clobbers whatever the foreground
  session had not committed. Both are survivable in an isolated worktree and are
  not in a shared one.

  When you land, fast-forward `main` with `git update-ref` rather than checking
  it out — the main worktree usually holds somebody else's branch, and checking
  `main` out there is the same collision from the other side. Verify first that
  `origin/main` is an ancestor of what you are landing and that everything above
  it is yours.

- **Never add AI attribution to a git object.** No `Co-Authored-By:` naming an
  assistant, no "Generated with …" line, no session URL, no robot marker, in
  commit messages or PR descriptions — whichever agent is writing them. The
  commit log stays clean. `.git/hooks/commit-msg` enforces this, and routing the
  message through a file or an editor does not get past it.

- **Repo artifacts are English.** README, comments, docs, CLI strings, commit
  messages. Conversation with the user is Korean; nothing that lands in the tree
  is.

## Before you claim a change works

- Run the whole suite, including `./scripts`, which carries the release gates —
  the citation gate reads every tracked file, so a test name quoted in a comment
  or in `CHANGELOG.md` must exist and must not be split by a line wrap.
- Mutate the fix and watch the test go red. A test written against a bug can
  pass under the bug it was written for. Confirm the mutation actually landed
  (`git diff --stat`): a `sed` that matches nothing exits 0, and green then
  reads as a blind test when nothing was mutated.
- Commit before mutating, or `git checkout --` takes the implementation with it.

## Releasing

`docs/` is excluded from git and exists only in the main worktree. The release
procedure lives with the maintainer, not here; what matters in this file is the
order: cut `CHANGELOG.md`, push `main`, wait for a green `ci.yml` run **on that
exact sha**, and only then tag. The release workflow's gate refuses a tag whose
commit has no green run, and an OS leg is usually where it fails — branches here
never reach origin, so the three-OS matrix only ever runs on `main` and anything
platform-specific is discovered after it lands.
