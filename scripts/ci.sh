#!/usr/bin/env bash
#
# Run the checks CI runs, here, before pushing.
#
# Usage:
#   scripts/ci.sh [check…]        # default: all
#
#   fmt    gofmt over every Go file the repository would publish
#   vet    go vet ./... for the host
#   test   go test ./... -race
#   cgo    every release target builds and vets with CGO_ENABLED=0
#   cites  no line points at a document this repository does not contain
#   plugin the plugin manifests, checked by the tool that owns their schema
#   all    all of the above, in that order
#
# `.github/workflows/ci.yml` calls these same subcommands, one per job, so a
# green run here and a green run there mean the same thing. There was no remote
# when this was written, which is exactly when a gate that lives only inside a
# workflow file is worth nothing.
#
# The one difference CI cannot reproduce locally: `test` runs on three
# operating systems there and on this one here. Everything else is identical.
#
# EXIT CODES, and the reason they are written here rather than only in a test:
#
#   0  every check asked for passed.
#   1  a check RAN and found a problem. Every failing check reports 1, whatever
#      the tool underneath it exited with.
#   2  usage: a check name this script does not have.
#
# Nothing else escapes, and `run_check` is what makes that true rather than a
# convention each check is trusted to keep. It was not true before: `gofmt -l`
# exits 2 on a file it cannot parse, `go test -race` exits 2 on an architecture
# without race support, `go build` exits 2 on an unsupported GOOS/GOARCH pair,
# `claude plugin validate` propagates whatever it likes, and `git` exits 128 --
# so `ci.sh fmt` over one unparseable Go file returned the code that means "you
# typed the check name wrong". Measured, all five. A number that means two
# things means neither.
#
# Two things that look like they belong here and do not:
#
#   * `CGO_ENABLED=0` at the top of this script. Every released binary is built
#     with it, but `go test -race` refuses to run without cgo
#     ("go: -race requires cgo; enable cgo by setting CGO_ENABLED=1"), so a
#     single exported value would silently turn one of the two checks off. The
#     variable is therefore set per check and never at file scope.
#   * A grep of `go list -deps` for `runtime/cgo`. Measured on this tree:
#     `CGO_ENABLED=1 go list -deps ./cmd/ccdad` reports `runtime/cgo` on
#     linux/amd64 under the host default while reporting nothing on
#     darwin/arm64 and windows/amd64, so that guard fails on a clean tree. The
#     honest question is whether the six CGO_ENABLED=0 builds succeed, and
#     whether the binary they produce records the setting — which is what `cgo`
#     below asks.

set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
cd -- "$repo_root"

# The six release targets, and the same list scripts/build-release.sh ships.
# They are deliberately not shared through a third file: this one asks whether
# the tree compiles for a target, that one names an asset, and a single list
# would make every future divergence a conflict between two questions.
targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

# Deliberately not a `local` in check_cgo with a RETURN trap: an EXIT trap runs
# after the function has returned, where a local is already out of scope and
# `set -u` turns the cleanup itself into the script's error.
# Three variables, ONE trap. `trap … EXIT` replaces the handler rather than
# adding to it — bash keeps one per signal — so a second trap installed for the
# plugin check would silently stop the cgo directory from ever being removed.
#
# All three are set HERE, above the trap, and not beside the code that uses
# them: `set -u` makes an unset variable inside the handler the script's error,
# which would turn any early abort into a message about the cleanup rather than
# about the check. `group_open` belongs to `group`/`endgroup` far below and is
# declared up here for exactly that reason.
cgo_tmp=
plugin_tmp=
group_open=
cleanup() {
	# First, because the fold has to close even when the script is dying: see
	# the note above `group`.
	endgroup
	if [ -n "$cgo_tmp" ]; then
		rm -rf -- "$cgo_tmp"
	fi
	if [ -n "$plugin_tmp" ]; then
		rm -rf -- "$plugin_tmp"
	fi
	return 0
}
trap cleanup EXIT

# ::group:: folds the section in the Actions log and is inert everywhere else,
# so the same output reads well in both places. Every `ci: …` line this script
# writes is printed after the fold has closed, because a fold that is never
# closed hides the one line the reader came for.
#
# That was ALL the pairing this file had, and it was not enough. The `ci: …`
# lines are only half the output: the other half is the tool's own diagnostic —
# gofmt's parse error, go vet's finding, the compiler's message — and that is
# printed by the tool, INSIDE the fold, on the path where `set -e` then kills
# the script before `endgroup` runs. Measured on this tree at the time this was
# written: seven group/endgroup pairs, and five of them can be aborted between
# the halves ON THE CHECK'S ORDINARY FAILURE PATH, vet and test included. The
# comment that used to sit here claimed the invariant held; it held for the
# lines this script writes and for nothing else.
#
# So the state is tracked and the close is idempotent, which makes three
# separate paths close the fold: `endgroup` at the end of a check, `group`
# opening the next one, and `cleanup` on the way out under `trap … EXIT`. The
# last of those is the one that covers `set -e`, a signal, and any `return`
# added inside a fold by a future check.
group() {
	# A dangling fold from an aborted check, closed before another opens: two
	# `::group::` markers with one `::endgroup::` between them nest in the
	# Actions log rather than sitting side by side.
	endgroup
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		echo "::group::$*"
	else
		echo "== $*" >&2
	fi
	group_open=1
}

endgroup() {
	if [ -z "$group_open" ]; then
		return 0
	fi
	group_open=
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		echo "::endgroup::"
	fi
}

# run <what> <command…> — the tool, its status made this script's business.
#
# Two jobs, and a check that shells out to anything should use it for both.
#
# It CLOSES THE FOLD on any failure, before a word is said about it. The tool
# has already written its diagnostic inside the fold; everything after that
# point, this script's own summary included, belongs outside.
#
# And it collapses every status that is not 0 or 1 down to 1, naming the real
# number. The codes that reach here are not hypothetical: `gofmt -l` exits 2 on
# a file it cannot parse, `go test -race` exits 2 with "-race is not supported
# on linux/386", `go build` exits 2 on an unsupported GOOS/GOARCH pair, `git`
# exits 128, a tool missing from PATH is 127, and `claude plugin validate`
# propagates whatever it was given. Every one of those used to leave through
# `set -e` as the SCRIPT's exit code, and 2 is the code this script reserves for
# a check name it does not have. The number is not thrown away — it is printed,
# because "gofmt exited 2" is the sentence that tells a reader to look for a
# file that does not parse rather than for a typo in a workflow file.
#
# `"$@" || rc=$?` and not a bare call: `set -e` is suppressed for the left-hand
# side of `||`, which is what makes the status readable here instead of being
# the answer. The `return 1` below is then a plain failure that `set -e` in the
# caller propagates exactly as before.
run() {
	local what=$1 rc=0
	shift
	"$@" || rc=$?
	if [ "$rc" -eq 0 ]; then
		return 0
	fi
	endgroup
	if [ "$rc" -eq 1 ]; then
		return 1
	fi
	echo "ci: $what exited $rc; the check ran, so this is reported as 1 — 2 means a check name this script does not have" >&2
	return 1
}

# gofmt over the files the repository actually carries, rather than `gofmt -l .`
# over the working tree. A checkout is not the only thing that lives under the
# repository root: worktrees, scratch checkouts and build output all appear
# there, and `gofmt -l .` would report files belonging to another branch and
# exit 1 on a tree that is perfectly clean.
check_fmt() {
	group "gofmt"
	# The list is captured BEFORE it is split, because the exit status of a
	# process substitution is one `set -e` cannot see. Written as
	# `done < <(git ls-files …)` this loop left a git that REFUSED — a directory
	# that is not a repository is the easy way to produce one — reporting
	# "ci: no Go files to format" and exiting 0. A check that looked at nothing
	# and said so quietly is the exact failure this file exists against, and it
	# is worse than the over-reporting below, because it is green.
	local listed files=() f
	if ! listed=$(git ls-files --cached --others --exclude-standard -- '*.go'); then
		endgroup
		echo "ci: git could not list this tree, so no Go file was checked" >&2
		return 1
	fi
	# A here-doc rather than `<<<`: `<<<""` still feeds one empty line, which
	# would make files=("") and hand gofmt an empty path.
	if [ -n "$listed" ]; then
		while IFS= read -r f; do
			files+=("$f")
		done <<EOF
$listed
EOF
	fi
	# bash 3.2 — still what /bin/bash is on the macOS runner — expands
	# "${empty[@]}" to an unbound-variable error under `set -u`.
	if [ ${#files[@]} -eq 0 ]; then
		endgroup
		echo "ci: no Go files to format" >&2
		return 0
	fi
	# gofmt's status read explicitly rather than left to `set -e`, and this is
	# the line the exit-code convention at the top of this file was written for.
	# `gofmt -l` exits 0 for BOTH "clean" and "needs formatting", and 2 for "I
	# could not do my job" — a file that does not parse, and equally a path that
	# is in the index and gone from disk. Bare, that 2 propagated out of the
	# assignment and became the script's exit code: the usage code, from a check
	# that ran and found a real problem. It also aborted before `endgroup`.
	#
	# Whatever gofmt did manage to print is reported either way. On a tree
	# holding one unparseable file and one merely unformatted one, gofmt writes
	# the parse error to stderr AND the unformatted name to stdout and exits 2 —
	# and the bare assignment threw the second away, so the developer was never
	# told about the file that only needed `gofmt -w`.
	local unformatted gofmt_rc=0
	unformatted=$(gofmt -l "${files[@]}") || gofmt_rc=$?
	endgroup
	if [ "$gofmt_rc" -ne 0 ]; then
		echo "ci: gofmt exited $gofmt_rc; it could not read every file and its own message is above" >&2
		if [ -n "$unformatted" ]; then
			echo "ci: the files it did get to, and that need formatting:" >&2
			echo "$unformatted" >&2
			echo "ci: run: gofmt -w $unformatted" >&2
		fi
		return 1
	fi
	if [ -n "$unformatted" ]; then
		echo "ci: not gofmt-clean:" >&2
		echo "$unformatted" >&2
		echo "ci: run: gofmt -w $unformatted" >&2
		return 1
	fi
	echo "ci: ${#files[@]} Go file(s) are gofmt-clean" >&2
}

check_vet() {
	group "go vet"
	run "go vet" go vet ./...
	endgroup
}

# -race, and therefore cgo, set explicitly rather than inherited. A developer
# who ran `go env -w CGO_ENABLED=0` — a reasonable thing to do in a repository
# whose global constraint says exactly that — otherwise gets a hard failure
# here that no CI runner reproduces.
#
# -count=1 because actions/setup-go restores the build cache, and GOCACHE holds
# test RESULTS as well as objects. Without it a package whose inputs did not
# change replays a cached PASS, and "the matrix ran on three operating systems"
# stops being true the moment it matters.
check_test() {
	group "go test -race"
	# `env` rather than a `CGO_ENABLED=1 go test …` prefix assignment, because
	# `run` invokes its argument as "$@" and a prefix assignment is shell syntax
	# rather than a word. The variable is still scoped to this one command.
	#
	# The status matters here for a reason no other leg has: `go test` exits 2
	# for "-race is not supported on linux/386", which is a machine talking
	# about itself, and 1 for a failing test. `run` keeps those apart.
	run "go test -race" env CGO_ENABLED=1 go test -race -count=1 ./...
	endgroup
}

# The CGO gate: this fails if any target needs CGO.
#
# `go build ./...` compiles every package, including the ones cmd/ccdad does
# not import — which is the whole point on Windows, where internal/cclink's
# _windows.go files and every _windows.go the daemon adds are otherwise
# compiled by nothing. `go vet` then type-checks the _test.go files too, which
# `go build` never looks at.
#
# Finally the binary itself is asked what it was built with. The two builds
# above prove the tree does not NEED cgo; `go version -m` proves the artefact
# was not quietly linked with it anyway.
check_cgo() {
	cgo_tmp=$(mktemp -d)

	local target goos goarch ext out info
	for target in $targets; do
		goos=${target%/*}
		goarch=${target#*/}
		ext=
		if [ "$goos" = windows ]; then
			ext=.exe
		fi
		out=$cgo_tmp/ccdad-$goos-$goarch$ext

		group "CGO_ENABLED=0 $goos/$goarch"
		# `env`, and through `run`, for the reason check_test states: `go build`
		# exits 2 on `unsupported GOOS/GOARCH pair`, which is a typo in the
		# `targets` list above rather than a tree that needs cgo, and 2 is this
		# script's usage code.
		run "go build $goos/$goarch" env GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build ./...
		run "go vet $goos/$goarch" env GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go vet ./...
		run "go build -o $goos/$goarch" env GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -o "$out" ./cmd/ccdad
		# Read into a variable rather than piping into `grep -q`: grep exits at
		# the first match, `go version` then dies of SIGPIPE, and `pipefail`
		# turns a successful check into a failing pipeline.
		#
		# Its status is taken explicitly for the same reason `run` exists; it
		# cannot go through `run` because the output is wanted in a variable.
		local version_rc=0
		info=$(go version -m "$out") || version_rc=$?
		endgroup
		if [ "$version_rc" -ne 0 ]; then
			echo "ci: go version -m exited $version_rc; $goos/$goarch built but could not be read back" >&2
			return 1
		fi
		case $info in
		*CGO_ENABLED=0*) ;;
		*)
			echo "ci: $goos/$goarch built, but the binary does not record CGO_ENABLED=0:" >&2
			echo "$info" >&2
			return 1
			;;
		esac
	done
	echo "ci: six targets build, vet and record CGO_ENABLED=0" >&2
}

# Every line this repository ships must stand on its own.
#
# NOT "every comment", which is what this said for a long time while doing
# something else. `git grep` matches a line, and no arm of this check has ever
# known what a comment is: a Go string literal carrying `§7.2` fails it, and so
# does plain README prose. That is the right scope and it is now the stated one
# — an unresolvable citation in the text of a user-visible error is worse than
# one in a comment, not better — but the words had to catch up with the code,
# because a gate whose failure message describes a narrower rule than it
# enforces teaches the reader to argue with it.
#
# This tree was written against a design document that is NOT published and is
# not going to be, and for a while its comments cited it by section — `§7.2`,
# `§9.3` — and named internal work items: "the brief", "task 47". Every one of
# those resolved for exactly one person. Stripping them out and writing the fact
# in was a day of work across a hundred and forty files; this is what stops it
# growing back one comment at a time.
#
# The gate is deliberately narrow: `§`, "the brief" and "task <n>" — the three
# forms literal enough to grep for without false positives. Prose that merely
# alludes to a document ("the spec fixes no number") is not greppable, so it is
# a review question rather than a gate — CONTRIBUTING.md carries the rule.
#
# A section symbol pointing at a PUBLISHED standard is fine and several ship
# here — `RFC 6749 §6` resolves for anyone with a browser, which is the whole
# distinction this gate is drawing. `cites_standard` below is that exemption.
# Any other exception belongs here too, in the open, rather than in a comment
# nobody sees.
#
# The file set all three shapes below are searched over: EVERYTHING, minus the
# three exclusions. An ARRAY rather than the space-separated string `targets`
# is: these are git pathspecs, and a pattern left unquoted would be expanded by
# the shell before git ever saw it.
#
# It used to be five extensions — `*.go *.sh *.ps1 *.yml *.md` — and that is a
# narrower rule than the one CONTRIBUTING.md states and than the one this
# check's own name claims. Measured at the time this was widened, each with a
# violation injected and the check still exiting 0: `Dockerfile`,
# `ccdad-entrypoint` (a shell script with no extension), the five
# `internal/cli/testdata/cmdshim/*.cmd`, `scripts/gen-cmd-shim-fixtures.js`,
# `THIRD-PARTY-LICENSES.txt`, `internal/relsign/testdata/sums.txt`,
# `.claude-plugin/marketplace.json`, `plugins/.claude-plugin/plugin.json`,
# `plugins/.mcp.json`, `LICENSE` and `NOTICE`. Thirty-seven tracked files sat
# outside the gate.
#
# MEASURED COST OF WIDENING IT, because a pathspec nobody counted is a pathspec
# nobody knows the price of: over all 513 tracked files, the spelled arm matches
# five lines and all five are RFC citations the exemption already allows, the
# pointer arm matches NOTHING, and the docpath arm matches one line —
# `.github/ISSUE_TEMPLATE/config.yml`'s `See SECURITY.md.` — which resolves. The
# whole of the widening costs zero new failures on this tree. The one thing it
# did cost is handled by `-I` where the greps are run; see the note there.
#
# Three files are excluded and all three for ONE reason: they have to contain
# the very strings this check fails on. ci.sh holds the patterns, ci_sh_test.go
# holds the fixtures that prove they fire, and CONTRIBUTING.md quotes them while
# stating the rule — and a gate that cannot state what it forbids is worse than
# the exclusion. The cost is real and worth naming: a genuinely unreachable
# citation written inside one of those three is not caught by anything.
# Measured, so that nobody tidies one away: dropping `:!CONTRIBUTING.md` fails
# the gate on four of its own lines, and dropping `:!scripts/ci_sh_test.go`
# fails it on seven fixtures. Neither is caught by any test, because every
# fixture repository is built fresh and contains neither file.
#
# The leading `.` is not decoration. A pathspec made only of exclusions means
# "everything but these" to a git new enough to say so, and the script has
# already `cd`-ed to the repository root, so `.` states the positive half in a
# form every git agrees about.
cites_paths=('.' ':!scripts/ci.sh' ':!scripts/ci_sh_test.go' ':!CONTRIBUTING.md')

# The SPELLED shape: three literals, each of which is how one class of
# unreachable reference was actually written here.
#
# NO \b. It is a GNU extension outside a bracket expression, not POSIX ERE, and
# macOS's git — built against the system regex library, not glibc's — treats it
# as a literal 'b' rather than a boundary, so `\bthe brief\b` becomes "bthe
# briefb" and never matches. That silently disabled this whole check on the
# macOS leg of CI: every case that expects a MISS still misses, by coincidence,
# and only the ones that expect a HIT go quiet. Measured on this tree with \b
# dropped: the same four lines match as matched with it, because nothing here
# has "the brief" or "task N" as a substring of a longer word.
cites_spelled='§|[Tt]he brief|[Tt]ask [0-9]+'

# The exemption: a section of a PUBLISHED standard, which resolves for anyone
# with a browser. This is subtracted from a line before the line is judged, and
# both halves of that sentence are the fix to what was here before.
#
# What was here before was `grep -vE 'RFC [0-9]+ §'` applied to whole LINES, and
# it was wrong in both directions. Measured, all of it:
#
#   * It failed correct prose. `See RFC 6749, §6 for the flow.` and
#     `See §6 of RFC 6749.` are citations CONTRIBUTING.md permits, and both went
#     red — a comma after the number, or the section written first, was enough.
#     So were `[RFC 6749] §6`, `RFC6749 §6`, a doubled space, and lowercase
#     `rfc`. A gate that fails on correct prose is a gate somebody switches off,
#     and the escape hatch it pushes people toward — `RFC 6749 Section 6`, no §
#     at all — is invisible to this check in both directions.
#   * It laundered real violations. Because the drop was per-LINE and ran after
#     the whole spelled arm, `RFC <n> §` anywhere on a line exempted that line
#     from ALL THREE literals: `// This is what task 47 asked for; the loopback
#     rule is RFC 8252 §7.3.` passed, and so did the same line with "the brief".
#     Either half alone failed. A string literal on the same line worked as the
#     laundering vehicle just as well as a comment.
#
# The comment that used to sit beside the filter called the laundering a
# deliberate trade, on the grounds that "the alternative is a lookbehind no
# portable grep has". That is not the alternative. Removing the accepted
# citations from a COPY of the line and re-testing what is left needs no
# lookbehind, is one `awk`, and costs nothing here — see check_cites.
#
# MEASURED ON THIS TREE: five lines carry a `§`, all in internal/oauth, all of
# the form `RFC <n> §<sect>`, and no line anywhere carries "the brief" or
# "task <n>". Every variant of this pattern that was tried accepts all five, so
# the cost of the change on today's content is zero lines and the whole of its
# effect is on lines nobody has written yet.
#
# `(§§|§)` and NOT `§+`, and this is the same trap as the `\b` two patterns
# below refuse. `§` is two bytes in UTF-8, and a byte-oriented regex engine —
# which is what a POSIX awk or a `sed` under LC_ALL=C is — reads `§+` as
# `\302\247+`, one `\302` followed by one or more `\247`. `§§` would then match
# only its first half and the exemption would silently stop working on that leg
# while every test that expects a MISS still missed. An alternation of two
# LITERALS has no such reading: both branches match byte for byte on every
# engine. Longer branch first, because a leftmost-longest guarantee is not one
# every engine gives. `§§6 and 7` is how a range is written, which is the only
# reason the two-symbol branch is here at all.
#
# The section must start with a DIGIT: without that, `RFC 9999 § anything at
# all` is an exemption that swallows the rest of the sentence, which is exactly
# the laundering shape this pattern exists to close. The RFC number is not
# checked for existence — `RFC 9999` is exempt — because whether a number was
# ever assigned is not a question a grep can ask, and pretending otherwise would
# be a gate that lies about what it verified.
#
# The Go specification is NOT here, and CONTRIBUTING.md names it in the same
# breath as the RFC. Measured: nothing in this tree cites it by section, so the
# exemption would be prose with no reader. A section of the Go specification is
# written `§Conversions` — a NAME, not a number — so it needs its own pattern
# rather than a widening of this one, and it should be written the day the first
# citation to it is.
cites_standard='[Rr][Ff][Cc] *[0-9]+,? *(§§|§)[0-9]+(\.[0-9]+)*|(§§|§)[0-9]+(\.[0-9]+)* *of *[Rr][Ff][Cc]'

# The POINTED shape, and it is the one that got past the first. `§`, "the
# brief" and "task n" are SPELLINGS -- a literal catches each. A document
# referred to by its bare NAME has no spelling to key on, so what is left to key
# on is the pointing phrase in front of it plus a target shaped like a slug:
# "see claude-code-oauth-ground-truth", which is how the one that got through
# was written. It sat in internal/cclink/keychain.go, pointing at a file in one
# person's notes directory, and the measurement it pointed at is now in that
# header instead.
#
# MEASURED ON THIS TREE, because a pattern nobody counted is a pattern nobody
# knows the cost of. Re-measured when the file set was widened, and every one of
# these numbers had moved, which is why they are dated rather than stated as
# facts about the pattern:
#
#   * The phrase-plus-slug form matches ZERO lines today. It matched exactly one
#     when it was written -- the keychain.go pointer above -- and that line was
#     replaced by the measurement it pointed at.
#   * The slug half WITHOUT the pointing phrase matches 973 lines, in 328
#     distinct shapes. It matched 29 when this was written. The corpus is not
#     what it was either: it is now dominated by release asset names
#     (`ccdad-linux-amd64`, 54 lines), ISO dates (`2026-08-22`, 43) and token
#     prefixes (`sk-ant-api03`, 42) rather than by prose. Dropping the phrase
#     requirement would report 918 of those 973.
#
# That second number is what the phrase is buying, and it is one test deep --
# TestCICitesDoesNotReportOrdinaryHyphenatedEnglish is the only thing that goes
# red if the alternation is deleted.
#
# THE ARITY, `{2,}`, IS A MEASUREMENT AND NOT A GUESS, and it was held by nothing
# until it was written down here. Relaxing it to `{1,}` leaves every test in the
# suite green and fails SEVEN correct lines of this tree, every one of them
# ordinary English behind the phrase `per`: `per sub-key`, `per rate-limit
# window` (twice), `per five-hour cycle`, `per five-hour window`, `per warm-up`,
# and `sees u-1's`. Six of the seven are `per`. Tightening it to `{3,}` is free
# today and stops catching `see keychain-notes-here`. So two segments is the
# floor at which the phrase form is still worth having, and both directions are
# now pinned by fixtures rather than by this paragraph.
#
# The cost of that floor, stated because it is a real miss rather than a
# rounding error: `see keychain-notes` -- one hyphen -- is not caught. A
# two-segment name is indistinguishable from `per rate-limit` by shape alone,
# and this gate would rather miss one than fail seven.
#
# `A-Z` is in both classes, and that was free: measured over the widened file
# set the pattern matches nothing with it that it did not match without it, and
# `see Keychain-Notes-Here` used to walk straight through. `_` is NOT, and the
# measurement is the reason -- adding it reports
# `see keychain_security_test.go.`, which is a correct citation of a tracked
# file, because the slug arm resolves its target without stripping a sentence's
# full stop the way the docpath arm does. One false positive is the whole price
# of a shape nobody here writes, so it is not paid.
#
# NO leading \b, for the reason cites_spelled above has none: macOS's git reads
# it as a literal 'b', not a boundary, which made this whole arm a silent no-op
# on that leg — `pointers=$(git grep ...)` never matched, so the loop below it
# never ran and unresolved stayed empty no matter what the tree contained.
# Measured with it dropped: still zero matches over this tree, same as with it.
cites_pointer='([Ss]ee|[Pp]er|[Rr]efer to|[Dd]escribed in|[Dd]ocumented in) [A-Za-z0-9]+(-[A-Za-z0-9]+){2,}'

# The POINTED shape written as a PATH, and the reason the shape above cannot see
# it: that pattern keys on a slug immediately after the pointing phrase, and
# `docs/plans/2026-08-25-self-upgrade.md` ends its first token at the slash
# before a single hyphen group has matched. Nothing in it matches, so a plan
# cited the way a plan is actually cited walked straight through -- which is how
# several comments here came to satisfy this gate while pointing at a file no
# reader outside one machine has.
#
# `docs/` is the case that matters and it is not a guess: it is in
# .git/info/exclude, and `git log --all --diff-filter=A -- 'docs/*'` is empty,
# so no commit in this repository has ever carried a file under it.
#
# What this arm has caught here so far is NOTHING, and that is worth stating
# rather than dressing up: across every commit reachable from every ref and
# every live worktree, the pattern has matched one line, `See SECURITY.md.`,
# and that one resolves. This is a gate against a shape that got past the
# other two, not a report of damage already done.
#
# MEASURED ON THIS TREE, because a pattern nobody counted is a pattern nobody
# knows the cost of. A pattern that flagged ANY unresolved path matched six
# lines and four of them were correct: `os/types_windows.go` is the Go standard
# library, `tools/call` is a method name on the wire, `internal/cli` is a
# directory, and `internal/identity/oauth.go.` is a real file with the
# sentence's full stop stuck to it. This gate is about DOCUMENTS -- whether a
# comment names a code path correctly is a different question with a different
# owner -- so the target must look like one: under `docs/`, or ending `.md`.
# Restricted that way it matches exactly one line here, `See SECURITY.md.` in
# .github/ISSUE_TEMPLATE/config.yml, and that one resolves.
# The `.md` half ends at a character that cannot continue a path, or at the end
# of the line. Without that boundary `guide.mdx` matches as the prefix
# `guide.md`, and a correct citation of a tracked file is reported for a name
# nobody wrote.
cites_docpath='([Ss]ee|[Pp]er|[Rr]efer to|[Dd]escribed in|[Dd]ocumented in) (docs/[A-Za-z0-9._/-]+|[A-Za-z0-9._/-]+\.md([^A-Za-z0-9]|$))'

# tracked_has answers whether the repository contains this exact path.
#
# Matched in the shell rather than through `git ls-files | grep -q`. That is the
# pipeline the comment above `go version -m` in check_cgo warns about: grep
# exits at the first match, the writer dies of SIGPIPE, and `pipefail` turns a
# pointer that RESOLVED into a failed script. The slug arm was written that way
# and never fired only because this repository's file list is smaller than a
# pipe buffer; it reads the same list from a here-string now, which is the same
# question asked without a writer to kill.
tracked=
tracked_has() {
	case $1 in
	*/*)
		# A PATH names one file and is matched whole. `(^|/)name` matching here
		# would let `docs/x.md` resolve against an unrelated `vendor/docs/x.md`.
		case $'\n'"$tracked"$'\n' in
		*$'\n'"$1"$'\n'*) return 0 ;;
		esac
		;;
	*)
		# A BARE NAME is how a person names a document, and the slug arm above
		# already resolves one that way. Matching it only at the repository root
		# failed `see PULL_REQUEST_TEMPLATE.md` -- the file is tracked, in
		# .github/ -- and CONTRIBUTING.md promises that pointing at a file in
		# the tree is fine.
		case $'\n'"$tracked"$'\n' in
		*$'\n'"$1"$'\n'* | *"/$1"$'\n'*) return 0 ;;
		esac
		;;
	esac
	return 1
}

# The one `git grep` all three arms below go through, so that the two flags that
# decide WHAT IS LOOKED AT are stated once and cannot drift between arms.
#
# `-I` skips files git considers binary, and it is what makes the widened
# pathspec safe. Without it `assets/ccdaddy.png` — 263KB whose bytes happen to
# contain the § sequence, and the only tracked file .gitattributes marks binary
# — turns the gate red with the single line `Binary file assets/ccdaddy.png
# matches`. That has no `file:line:` prefix and no offending text, so it is a
# failure the reader cannot act on. Excluding the one path would have worked
# today and broken on the next image; asking git not to grep binaries is the
# question actually being asked.
#
# `--untracked` searches the file you have just written and not yet staged.
# check_fmt has always done this, through `git ls-files --cached --others
# --exclude-standard`, and the asymmetry was a real hole: a new file carrying a
# citation gave exit 0, and the SAME file after `git add -N` gave exit 1, so
# running this before staging was a green that meant nothing. `--exclude-
# standard` is git's default under `--untracked` and is passed anyway, because a
# gate that reads the working tree should say out loud that it stops at
# .gitignore.
cites_grep() {
	git grep -I --untracked --exclude-standard -nE "$@"
}

# cites_search runs one arm's pattern and keeps the difference between "found
# nothing" and "could not ask". The result lands in `cites_found`; a return of 1
# means git refused.
#
# `git grep` exits 1 for no matches and something larger for an error, and
# `if hits=$(git grep …)` reads both as falsy — so an arm whose pattern the
# platform's regex library rejects reported a clean tree and exited 0.
# Reproduced: an unbalanced `{` in cites_pointer makes git print
# `fatal: command line, '…': Unmatched \{` and this check then says
# "no line cites a document this repository does not contain", rc=0.
#
# That is the `\b` trap two patterns below already carry, with a louder cause
# and the same shape: the gate is off on some leg and green about it. macOS's
# git and GNU git do not accept exactly the same extended regular expressions,
# which is the whole reason this can happen on one runner and not another.
cites_found=
cites_search() {
	local rc=0
	cites_found=$(cites_grep "$@" -- "${cites_paths[@]}") || rc=$?
	if [ "$rc" -le 1 ]; then
		return 0
	fi
	echo "ci: git grep exited $rc for this pattern, so that arm of the check read nothing:" >&2
	echo "ci:   $1" >&2
	return 1
}

check_cites() {
	group "self-contained citations"
	local spelled spelled_raw pointers unresolved line targets target hits docpaths
	# Read once, and matched below without a pipe. See tracked_has.
	#
	# The same universe the greps search, for one reason: a pointer at a file
	# you have just written should resolve. Reading the index here while
	# searching the working tree would report `see new-thing.md` as unreachable
	# in the one minute between writing that file and staging it.
	#
	# The status is taken rather than left to `set -e`, which is the same fix
	# check_fmt's file list needed: bare, a git that refused left this check with
	# git's own 128 as the SCRIPT's exit code. 128 is a worse collision than the
	# 2 that started this — it is the range a shell also uses for "killed by a
	# signal" — and it arrived with the fold still open.
	if ! tracked=$(git ls-files --cached --others --exclude-standard); then
		endgroup
		echo "ci: git could not list this tree, so nothing was checked" >&2
		return 1
	fi
	# THE EXEMPTION IS SUBTRACTED, NOT USED TO DROP THE LINE. What was here was
	# `| grep -vE 'RFC [0-9]+ §'`, which threw away the whole LINE and ran after
	# the whole spelled arm, so `RFC <n> §` anywhere on a line exempted that line
	# from all three literals at once -- `// This is what task 47 asked for; the
	# loopback rule is RFC 8252 §7.3.` passed while either half alone failed.
	#
	# Removing the accepted citations from a COPY and re-testing the remainder
	# needs no lookbehind, which is what the comment that used to sit here said
	# the alternative required. A line is judged on what it says OTHER than its
	# citations, which is the rule anybody would state in words.
	#
	# Per line rather than in one pass, matching the two arms below: this arm's
	# output is five lines on this tree, and a `sed -E` per line is the same
	# engine the rest of this check already uses rather than a second regex
	# dialect to be wrong about on one platform.
	spelled=
	if ! cites_search "$cites_spelled"; then
		endgroup
		return 1
	fi
	spelled_raw=$cites_found
	if [ -n "$spelled_raw" ]; then
		while IFS= read -r line; do
			if printf '%s\n' "$line" | sed -E "s/$cites_standard//g" |
				grep -qE "$cites_spelled"; then
				spelled="${spelled}${line}
"
			fi
		done <<EOF
$spelled_raw
EOF
	fi

	# A POINTER WHOSE TARGET THIS REPOSITORY CONTAINS IS ALLOWED, and that is
	# the rule rather than a concession to it: CONTRIBUTING.md's "Style" permits
	# a comment to point at a file in this tree. Without this arm the gate would
	# fail on "see gen-cmd-shim-fixtures" the first time somebody wrote it, and
	# a gate that fails on correct code is a gate somebody switches off.
	unresolved=
	if ! cites_search "$cites_pointer"; then
		endgroup
		return 1
	fi
	pointers=$cites_found
	if [ -n "$pointers" ]; then
		while IFS= read -r line; do
			# Every target on the line, not the first: one unresolvable
			# pointer is enough, and taking only `head -1` would let a
			# second pointer on the same line through.
			#
			# `|| true` for the reason the docpath arm below spells out, and it
			# was missing here: this is byte-for-byte the same construct, and
			# without the guard a line `grep -oE` cannot re-match kills the
			# check under `set -e` with nothing printed at all -- exit 1 and an
			# empty report, indistinguishable from a finding except that there
			# is no finding. `-I` above makes the binary case unreachable now;
			# the guard stays because it costs nothing and the next reason a
			# line fails to re-match will not announce itself either.
			targets=$(printf '%s\n' "$line" | grep -oE "$cites_pointer" |
				sed -E 's/.*[[:space:]]//' || true)
			for target in $targets; do
				if grep -qE "(^|/)${target}(\.[A-Za-z0-9]+)?\$" <<<"$tracked"; then
					continue
				fi
				unresolved="${unresolved}${line}
"
				break
			done
		done <<EOF
$pointers
EOF
	fi

	# A DOCUMENT POINTED AT BY PATH, resolved exactly rather than by suffix: a
	# path names one file, and `(^|/)name` matching would let `docs/x.md`
	# resolve against an unrelated `vendor/docs/x.md`.
	docpaths=
	if ! cites_search "$cites_docpath"; then
		endgroup
		return 1
	fi
	pointers=$cites_found
	if [ -n "$pointers" ]; then
		while IFS= read -r line; do
			# The pointing phrase is stripped by NAME rather than by taking
			# the last whitespace-delimited token: the `.md` half now ends on a
			# boundary character, and that character can be a space, which
			# would leave the last token empty.
			#
			# Then the trailing strip, which BOTH halves of the pattern need.
			# The `docs/` branch has `.` inside its character class, so
			# `see docs/plans/a-thing.md.` ending a sentence yields the target
			# with the full stop attached, which resolves to nothing and fails a
			# correct line.
			#
			# The comment here used to say the `.md` half was exempt, on the
			# grounds that its match has to end at `.md`. That stopped being
			# true when the half gained its `([^A-Za-z0-9]|$)` boundary: the
			# boundary CONSUMES the character it matches, so `see SECURITY.md.`
			# yields `see SECURITY.md.` -- measured with `grep -oE` against the
			# pattern as it stands. Deleting the strip now fails that line, and
			# `See SECURITY.md.` is a line this repository actually carries, in
			# .github/ISSUE_TEMPLATE/config.yml.
			#
			# `|| true` because not every line git grep printed re-matches: a
			# NUL byte anywhere in a searched file makes it print
			# `Binary file X matches` with no `file:line:` prefix, `grep -oE`
			# then exits 1, and under `set -e` the assignment kills the check
			# with no diagnostic at all.
			targets=$(printf '%s\n' "$line" | grep -oE "$cites_docpath" |
				sed -E 's/^([Ss]ee|[Pp]er|[Rr]efer to|[Dd]escribed in|[Dd]ocumented in)[[:space:]]+//; s/[^A-Za-z0-9_/-]+$//' || true)
			for target in $targets; do
				if tracked_has "$target"; then
					continue
				fi
				docpaths="${docpaths}${line}
"
				break
			done
		done <<EOF
$pointers
EOF
	fi

	# Deduplicated: a hyphenated document name ending in `.md` matches the slug
	# pattern, which stops before the extension, AND this one -- and the same
	# file:line printed twice reads as two problems.
	hits=$(printf '%s\n%s\n%s' "$spelled" "$unresolved" "$docpaths" |
		grep -v '^$' | awk '!seen[$0]++' || true)
	endgroup
	if [ -n "$hits" ]; then
		echo "ci: these lines point at something no reader outside this machine has:" >&2
		echo "$hits" >&2
		echo "ci: state the fact instead — see CONTRIBUTING.md, \"Style\"" >&2
		return 1
	fi
	echo "ci: no line cites a document this repository does not contain" >&2
}

# The plugin manifests, checked by the tool that owns their schema.
#
# A schema written here would be this repository's belief about somebody else's
# schema, and it would drift with nothing to say so. So this shells out, and the
# version it shells out to is PINNED in .github/workflows/ci.yml: an unpinned
# install takes whatever is newest, and a warning added in any Claude Code
# release would then fail the release gate on a commit that changed nothing.
#
# Two paths, not one. The first validates the marketplace and recurses into the
# entry's plugin manifest. The second is the guard for a mistyped source: a
# source naming a directory that is not there passes the first silently, with
# nothing validated at all.
#
# Then the smoke, which is the only thing anywhere that proves Claude Code reads
# these manifests the way this repository believes it does: the validator never
# opens the file the plugin manifest names, and an inline server object reports
# zero servers in the plugin UI while the server actually runs. Installing into a
# throwaway config directory and asking how many servers were found is what
# separates those. It needs no credentials and no ccdad binary — a server that
# cannot be executed is still a server that was FOUND, and whether `ccdad mcp`
# speaks the protocol is proven by the Go suite instead.
check_plugin() {
	if ! command -v claude >/dev/null 2>&1; then
		if [ -n "${CCDAD_REQUIRE_CLAUDE:-}" ]; then
			echo "ci: CCDAD_REQUIRE_CLAUDE is set and claude is not on PATH; the plugin manifests would go back to being validated by nobody while this still reported green" >&2
			return 1
		fi
		echo "ci: claude is not installed; the plugin manifests are validated by nothing here" >&2
		return 0
	fi

	# Through `run`, and this is the leg where it matters most: `claude` is
	# somebody else's binary and its exit code is theirs to change. Measured
	# with fakes exiting 2, 3 and 7, ci.sh exited 2, 3 and 7 — so a validator
	# that started using 2 for its own purposes would have made this script say
	# "you typed the check name wrong" on every push.
	group "claude plugin validate --strict"
	run "claude plugin validate ." claude plugin validate --strict .
	run "claude plugin validate plugins" claude plugin validate --strict plugins
	endgroup

	plugin_tmp=$(mktemp -d)
	local details details_rc=0
	group "the installed plugin declares its server"
	run "claude plugin marketplace add" env CLAUDE_CONFIG_DIR=$plugin_tmp claude plugin marketplace add "$repo_root"
	run "claude plugin install" env CLAUDE_CONFIG_DIR=$plugin_tmp claude plugin install ccdad@ccdaddy
	details=$(CLAUDE_CONFIG_DIR=$plugin_tmp claude plugin details ccdad@ccdaddy) || details_rc=$?
	endgroup
	if [ "$details_rc" -ne 0 ]; then
		echo "ci: claude plugin details exited $details_rc; the plugin installed but could not be read back" >&2
		return 1
	fi
	case $details in
	*"MCP servers (1)"*) ;;
	*)
		echo "ci: the installed plugin declares no MCP server; the manifest names a file Claude Code did not read:" >&2
		echo "$details" >&2
		return 1
		;;
	esac

	echo "ci: both manifests validate --strict, and the installed plugin declares one MCP server" >&2
}

run_check() {
	case $1 in
	fmt) check_fmt ;;
	vet) check_vet ;;
	test) check_test ;;
	cgo) check_cgo ;;
	cites) check_cites ;;
	plugin) check_plugin ;;
	all)
		check_fmt
		check_vet
		check_test
		check_cgo
		check_cites
		# Last, because it is the only check that can legitimately skip and a
		# skip printed last is the one still on the reader's screen.
		check_plugin
		;;
	*)
		echo "ci: unknown check: $1" >&2
		echo "ci: usage: scripts/ci.sh [fmt|vet|test|cgo|cites|plugin|all …]" >&2
		return 2
		;;
	esac
}

if [ $# -eq 0 ]; then
	set -- all
fi
for check in "$@"; do
	run_check "$check"
done
