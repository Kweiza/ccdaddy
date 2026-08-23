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
cgo_tmp=
cleanup() {
	if [ -n "$cgo_tmp" ]; then
		rm -rf -- "$cgo_tmp"
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
# Two files are excluded, and they are the two that DESCRIBE this rule: this
# script, whose search pattern is the banned character, and CONTRIBUTING.md,
# which shows contributors what not to write. A gate that cannot state what it
# forbids is worse than the exclusion.
check_cites() {
	group "self-contained comments"
	local hits
	# The `grep -v` runs last, so a line carrying BOTH an RFC citation and a
	# real one escapes. That trade is deliberate: the alternative is a lookbehind
	# no portable grep has, and such a line has never existed here.
	hits=$(git grep -nE '§|\b[Tt]he brief\b|\b[Tt]ask [0-9]+' -- \
		'*.go' '*.sh' '*.ps1' '*.yml' '*.md' \
		':!scripts/ci.sh' ':!CONTRIBUTING.md' |
		grep -vE 'RFC [0-9]+ §' || true)
	endgroup
	if [ -n "$hits" ]; then
		echo "ci: these comments point at something no reader outside this machine has:" >&2
		echo "$hits" >&2
		echo "ci: state the fact instead — see CONTRIBUTING.md, \"Style\"" >&2
		return 1
	fi
	echo "ci: no comment cites a document this repository does not contain" >&2
}

run_check() {
	case $1 in
	fmt) check_fmt ;;
	vet) check_vet ;;
	test) check_test ;;
	cgo) check_cgo ;;
	cites) check_cites ;;
	all)
		check_fmt
		check_vet
		check_test
		check_cgo
		check_cites
		;;
	*)
		echo "ci: unknown check: $1" >&2
		echo "ci: usage: scripts/ci.sh [fmt|vet|test|cgo|cites|all …]" >&2
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
