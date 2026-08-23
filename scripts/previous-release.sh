#!/usr/bin/env bash
#
# Name the release a post-release smoke test should upgrade FROM.
#
# Usage: scripts/previous-release.sh v1.2.3
#
# Writes one KEY=VALUE line to stdout, which is the shape $GITHUB_OUTPUT wants:
#
#   previous=v1.2.2
#
# Everything else goes to stderr. On a repository whose only release is the one
# being smoke-tested it writes `previous=` and exits 0: that is the FIRST
# release, and an upgrade leg with nothing to upgrade from has no work to do.
# Failing there would make the one release that most needs a smoke test the one
# release that cannot have one.
#
# Environment:
#   GH_REPO | GITHUB_REPOSITORY  owner/repo (Actions sets the second)
#   GH_TOKEN                     what `gh api` authenticates with
#
# Three decisions worth writing down, because each of them is a way this picks
# the wrong tag and still looks like it worked:
#
#   * The order is imposed here, by published_at, rather than taken from the
#     API. `GET /repos/{owner}/{repo}/releases` is documented to list newest
#     first and is relied on for WHICH hundred come back, but "the release
#     before this one" is a question about publication order and it is cheap to
#     say so rather than to inherit it.
#   * Drafts are excluded. A draft has no public download URL, so pinning
#     CCDAD_VERSION at one makes the installer's fail-closed checksum step
#     abort - and that abort reads as a broken installer rather than as a tag
#     that was never published.
#   * Prereleases are NOT excluded. Upgrading from an rc to the stable that
#     follows it is an upgrade real users perform, and it is this repository's
#     entire release history so far: v0.1.0 published over v0.1.0-rc1. Skipping
#     prereleases would have left the first stable release with no upgrade leg
#     on the one occasion there was something to upgrade from.
#
# A `gh api` that fails is fatal rather than an empty answer. "There is no
# earlier release" and "nobody could ask" are the same string to the caller,
# and the second one silently turns the upgrade leg off.

set -euo pipefail

tag=${1:-}
repo=${GH_REPO:-${GITHUB_REPOSITORY:-}}

if [ -z "$tag" ]; then
	echo "previous-release: no tag given" >&2
	echo "previous-release: usage: scripts/previous-release.sh <tag>" >&2
	exit 1
fi
if [ -z "$repo" ]; then
	echo "previous-release: no repository; set GH_REPO or GITHUB_REPOSITORY" >&2
	exit 1
fi

errfile=$(mktemp)
trap 'rm -f -- "$errfile"' EXIT

# Deliberately not --paginate: gh applies --jq to each page separately, so a
# filter that sorts would sort within a page and the aggregate would be
# whatever order the pages arrived in. One page of a hundred is every release
# this repository will have for years, and it is the newest hundred.
if ! tags=$(gh api "repos/$repo/releases?per_page=100" \
	--jq '[.[] | select(.draft | not)] | sort_by(.published_at) | reverse | .[].tag_name' 2>"$errfile"); then
	echo "previous-release: cannot list the releases of $repo" >&2
	cat "$errfile" >&2
	exit 1
fi

previous=
while IFS= read -r candidate; do
	[ -n "$candidate" ] || continue
	# The release being tested is in this list whenever it has been published,
	# and absent when the caller is racing ahead of it. Skipping it by name
	# covers both without the caller having to say which case it is in.
	[ "$candidate" != "$tag" ] || continue
	previous=$candidate
	break
done <<EOF
$tags
EOF

printf 'previous=%s\n' "$previous"
if [ -n "$previous" ]; then
	echo "previous-release: $tag upgrades from $previous" >&2
else
	echo "previous-release: no release before $tag — there is nothing to upgrade from" >&2
fi
