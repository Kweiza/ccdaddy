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
#   cites  no comment points at a document this repository does not contain
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
# Two variables, ONE trap. `trap … EXIT` replaces the handler rather than adding
# to it — bash keeps one per signal — so a second trap installed for the plugin
# check would silently stop the cgo directory from ever being removed.
cgo_tmp=
plugin_tmp=
cleanup() {
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
# so the same output reads well in both places. Nothing that reports a failure
# is printed inside a group: a fold that is never closed hides the one line the
# reader came for.
group() {
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		echo "::group::$*"
	else
		echo "== $*" >&2
	fi
}

endgroup() {
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		echo "::endgroup::"
	fi
}

# gofmt over the files the repository actually carries, rather than `gofmt -l .`
# over the working tree. A checkout is not the only thing that lives under the
# repository root: worktrees, scratch checkouts and build output all appear
# there, and `gofmt -l .` would report files belonging to another branch and
# exit 1 on a tree that is perfectly clean.
check_fmt() {
	group "gofmt"
	local files=() f
	while IFS= read -r f; do
		files+=("$f")
	done < <(git ls-files --cached --others --exclude-standard -- '*.go')
	# bash 3.2 — still what /bin/bash is on the macOS runner — expands
	# "${empty[@]}" to an unbound-variable error under `set -u`.
	if [ ${#files[@]} -eq 0 ]; then
		endgroup
		echo "ci: no Go files to format" >&2
		return 0
	fi
	local unformatted
	unformatted=$(gofmt -l "${files[@]}")
	endgroup
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
	go vet ./...
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
	CGO_ENABLED=1 go test -race -count=1 ./...
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
		GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build ./...
		GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go vet ./...
		GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -o "$out" ./cmd/ccdad
		# Read into a variable rather than piping into `grep -q`: grep exits at
		# the first match, `go version` then dies of SIGPIPE, and `pipefail`
		# turns a successful check into a failing pipeline.
		info=$(go version -m "$out")
		endgroup
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

# Every comment must stand on its own.
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
# distinction this gate is drawing. Those lines are filtered out by name. Any
# other exception belongs here too, in the open, rather than in a comment
# nobody sees.
#
# The file set all three shapes below are searched over. An ARRAY rather than
# the space-separated string `targets` is: these are git pathspecs, and `*.md`
# unquoted would be expanded by the shell into the five .md files at the
# repository root before git ever saw the pattern.
#
# Three files are excluded and all three for ONE reason: they have to contain
# the very strings this check fails on. ci.sh holds the patterns, ci_sh_test.go
# holds the fixtures that prove they fire, and CONTRIBUTING.md quotes them while
# stating the rule — and a gate that cannot state what it forbids is worse than
# the exclusion. The cost is real and worth naming: a genuinely unreachable
# citation written inside one of those three is not caught by anything.
cites_paths=('*.go' '*.sh' '*.ps1' '*.yml' '*.md'
	':!scripts/ci.sh' ':!scripts/ci_sh_test.go' ':!CONTRIBUTING.md')

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
# knows the cost of: the phrase-plus-slug form matched exactly one line, that
# one. The slug half WITHOUT the pointing phrase matched 29, nearly all of them
# ordinary hyphenated English -- "read-decide-merge-write",
# "sibling-temp-file-then-rename", "error-to-exit-code" -- which is why the
# phrase is required rather than the shape alone.
#
# NO leading \b, for the reason cites_spelled above has none: macOS's git reads
# it as a literal 'b', not a boundary, which made this whole arm a silent no-op
# on that leg — `pointers=$(git grep ...)` never matched, so the loop below it
# never ran and unresolved stayed empty no matter what the tree contained.
# Measured with it dropped: still zero matches over this tree, same as with it.
cites_pointer='([Ss]ee|[Pp]er|[Rr]efer to|[Dd]escribed in|[Dd]ocumented in) [a-z0-9]+(-[a-z0-9]+){2,}'

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

check_cites() {
	group "self-contained comments"
	local spelled pointers unresolved line targets target hits docpaths
	# Read once, and matched below without a pipe. See tracked_has.
	tracked=$(git ls-files)
	# The `grep -v` runs last, so a line carrying BOTH an RFC citation and a
	# real one escapes. That trade is deliberate: the alternative is a lookbehind
	# no portable grep has, and such a line has never existed here.
	spelled=$(git grep -nE "$cites_spelled" -- "${cites_paths[@]}" |
		grep -vE 'RFC [0-9]+ §' || true)

	# A POINTER WHOSE TARGET THIS REPOSITORY CONTAINS IS ALLOWED, and that is
	# the rule rather than a concession to it: CONTRIBUTING.md's "Style" permits
	# a comment to point at a file in this tree. Without this arm the gate would
	# fail on "see gen-cmd-shim-fixtures" the first time somebody wrote it, and
	# a gate that fails on correct code is a gate somebody switches off.
	unresolved=
	if pointers=$(git grep -nE "$cites_pointer" -- "${cites_paths[@]}"); then
		while IFS= read -r line; do
			# Every target on the line, not the first: one unresolvable
			# pointer is enough, and taking only `head -1` would let a
			# second pointer on the same line through.
			targets=$(printf '%s\n' "$line" | grep -oE "$cites_pointer" |
				sed -E 's/.*[[:space:]]//')
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
	if pointers=$(git grep -nE "$cites_docpath" -- "${cites_paths[@]}"); then
		while IFS= read -r line; do
			# The pointing phrase is stripped by NAME rather than by taking
			# the last whitespace-delimited token: the `.md` half now ends on a
			# boundary character, and that character can be a space, which
			# would leave the last token empty.
			#
			# Then the trailing strip, and the half it is for: the `docs/`
			# branch has `.` inside its character class, so
			# `see docs/plans/a-thing.md.` ending a sentence yields the target
			# with the full stop attached, which resolves to nothing and fails a
			# correct line. A test written around `See SECURITY.md.` was blind
			# to this -- the `.md` half cannot take a stop inside its match --
			# and deleting the strip left the suite green.
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
		echo "ci: these comments point at something no reader outside this machine has:" >&2
		echo "$hits" >&2
		echo "ci: state the fact instead — see CONTRIBUTING.md, \"Style\"" >&2
		return 1
	fi
	echo "ci: no comment cites a document this repository does not contain" >&2
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

	group "claude plugin validate --strict"
	claude plugin validate --strict .
	claude plugin validate --strict plugins
	endgroup

	plugin_tmp=$(mktemp -d)
	local details
	group "the installed plugin declares its server"
	CLAUDE_CONFIG_DIR=$plugin_tmp claude plugin marketplace add "$repo_root"
	CLAUDE_CONFIG_DIR=$plugin_tmp claude plugin install ccdad@ccdaddy
	details=$(CLAUDE_CONFIG_DIR=$plugin_tmp claude plugin details ccdad@ccdaddy)
	endgroup
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
