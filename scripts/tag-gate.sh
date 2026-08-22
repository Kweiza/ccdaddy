#!/usr/bin/env bash
#
# Decide whether a git tag may become a release, and say what it means.
#
# Usage: scripts/tag-gate.sh v1.2.3
#
# On an acceptable tag it writes two KEY=VALUE lines to stdout, which is the
# shape GITHUB_OUTPUT wants:
#
#   version=1.2.3
#   prerelease=false
#
# Everything else goes to stderr, so the caller can redirect stdout straight
# into $GITHUB_OUTPUT. On an unacceptable tag it writes nothing to stdout and
# exits 1.
#
# This runs in a job of its own, BEFORE the build. A tag that is not strict
# semver would otherwise be discovered at `gh release create`, which is the
# last step of the last job - after every build minute has been spent.

set -euo pipefail

tag=${1:-}
if [ -z "$tag" ]; then
	echo "tag-gate: no tag given" >&2
	exit 1
fi

# Strict semver, and deliberately no build metadata: a "+" survives git and
# then has to be percent-encoded in every URL that names the tag.
core='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
ident='(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)'
pre="${ident}(\\.${ident})*"

# Matched with bash's own =~ rather than by piping into grep. grep is
# line-oriented, so ^...$ against a value containing a newline matches the
# FIRST LINE and accepts the whole thing - which would then write two lines
# into $GITHUB_OUTPUT. Git will not create such a ref, but this is the one
# place where a repository-supplied string becomes a step output.
re="^v${core}(-${pre})?$"
if ! [[ $tag =~ $re ]]; then
	echo "tag-gate: $tag is not a strict semver tag (vMAJOR.MINOR.PATCH[-prerelease])" >&2
	exit 1
fi

version=${tag#v}

# A prerelease is published and then MARKED, so that
# github.com/.../releases/latest/download skips it. Both installers resolve
# "latest" through that path and neither calls the API, so marking is the whole
# mechanism by which a prerelease stays off the default install - and pinning
# CCDAD_VERSION is the whole mechanism by which someone can still ask for one.
prerelease=false
case "$version" in
*-*) prerelease=true ;;
esac

printf 'version=%s\n' "$version"
printf 'prerelease=%s\n' "$prerelease"
echo "tag-gate: $tag -> version $version, prerelease $prerelease" >&2
