#!/usr/bin/env bash
#
# Regenerate THIRD-PARTY-LICENSES.txt from the modules the released binaries
# actually link.
#
# Usage:
#   scripts/third-party-licenses.sh            # rewrite THIRD-PARTY-LICENSES.txt
#   scripts/third-party-licenses.sh -          # write to stdout instead
#
# Why this is a generated file and not a hand-written one: BSD-3-Clause and
# Apache-2.0 both require the notice to travel with a BINARY distribution, and
# ccdaddy ships static binaries. A hand-maintained list is wrong the first time
# somebody runs `go get`, and wrong silently — which is why
# third_party_licenses_test.go fails when this file is stale.
#
# Two decisions worth knowing before editing:
#
#   * The module set is the UNION over all six release targets, not the deps of
#     a host build. cobra pulls in mousetrap only on Windows, so a list computed
#     on Linux omits a module that three of the six published binaries link.
#     The targets here must match scripts/build-release.sh; the test asserts it.
#   * It reads `go list -deps` rather than go.mod's require block. go.mod lists
#     what the module graph needs, which includes test-only dependencies that
#     never reach a user's binary and therefore carry no notice obligation.
#     What is linked is the honest set.

set -euo pipefail

# Deterministic ordering, so a regeneration on another machine produces the
# same bytes and the staleness test compares content rather than locale.
export LC_ALL=C

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
cd "$repo_root"

# Keep in step with scripts/build-release.sh. Duplicated rather than sourced
# because that script does work on load; the test pins the two lists equal.
targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

die() {
	printf 'third-party-licenses: %s\n' "$1" >&2
	exit 1
}

command -v go >/dev/null 2>&1 || die "go is not on PATH"

# A cold module cache makes `go list` reach the network; do it once, loudly,
# rather than once per target inside the loop.
go mod download

modcache=$(go env GOMODCACHE)
[ -n "$modcache" ] || die "go env GOMODCACHE is empty"

# `and .Module .Module.Version` drops the standard library (no module) and the
# main module (a module with no version). What is left is exactly the
# third-party code linked into the binary.
mods=$(
	for target in $targets; do
		GOOS=${target%/*} GOARCH=${target#*/} \
			go list -deps -f '{{if and .Module .Module.Version}}{{.Module.Path}} {{.Module.Version}}{{end}}' ./cmd/ccdad
	done | sort -u
)
[ -n "$mods" ] || die "no third-party modules found; that cannot be right"

# Resolve every license file BEFORE emitting a byte. A `die` inside the emit
# loops below would run in a pipeline subshell and take only that subshell with
# it, leaving a truncated file behind and an exit status of zero -- which is the
# one failure mode a generated legal file must not have.
licenses=""
while read -r path version; do
	[ -n "$path" ] || continue
	# The module cache stores paths case-escaped: an upper-case letter becomes
	# "!" plus its lower-case form. Every module here is lower-case today, but a
	# future dependency with a capital in its path would silently miss its
	# license file without this.
	escaped=$(printf '%s' "$path" | sed 's/\([A-Z]\)/!\L\1/g')
	dir="$modcache/$escaped@$version"
	found=""
	for candidate in "$dir"/LICENSE "$dir"/LICENSE.txt "$dir"/LICENSE.md \
		"$dir"/COPYING "$dir"/COPYING.txt "$dir"/LICENCE; do
		if [ -f "$candidate" ]; then
			found=$candidate
			break
		fi
	done
	[ -n "$found" ] || die "no license file for $path@$version under $dir"
	licenses="$licenses$path $version $found
"
done <<EOF
$mods
EOF

emit() {
	cat <<'EOF'
Third-party licenses
====================

ccdaddy's own license is in LICENSE; the two projects it adapts design and
logic from are in NOTICE. This file is the third set: the Go modules that are
statically linked into every released ccdad binary. Each license is reproduced
verbatim below, which is what BSD-3-Clause and Apache-2.0 ask for when the
distribution is a binary.

GENERATED FILE — do not edit. Regenerate with:

    scripts/third-party-licenses.sh

The list is the union over all six release targets, so it includes modules a
build on your own machine may not link.

EOF

	while read -r path version _; do
		[ -n "$path" ] || continue
		printf '  %s %s\n' "$path" "$version"
	done <<EOF
$licenses
EOF
	printf '\n'

	while read -r path version license; do
		[ -n "$path" ] || continue
		printf -- '------------------------------------------------------------------------------\n'
		printf '%s %s\n' "$path" "$version"
		printf -- '------------------------------------------------------------------------------\n\n'
		# Byte-for-byte, including whatever trailing whitespace the upstream
		# file carries. A license reproduction that has been tidied is not a
		# reproduction, and a dependency that changes its own license text
		# SHOULD change this file -- that is the signal, not noise.
		cat -- "$license"
		# One blank line between entries regardless of whether the license file
		# ended with a newline, so the separator is a property of this script
		# rather than of the module.
		printf '\n\n'
	done <<EOF
$licenses
EOF
}

if [ "${1:-}" = "-" ]; then
	emit
else
	emit >"$repo_root/THIRD-PARTY-LICENSES.txt"
	printf 'third-party-licenses: wrote %s\n' "$repo_root/THIRD-PARTY-LICENSES.txt"
fi
