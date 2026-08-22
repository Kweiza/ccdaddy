#!/usr/bin/env bash
#
# Build the six release assets plus sha256sums.txt into a distribution
# directory. This is the single source of truth for asset names and for the
# link-time version stamp: both installers, the sums file, the build-provenance
# attestation subject and the post-release smoke test all compute from what
# this script writes, so a wrong name or a missing stamp is wrong in four
# places at once and is only discovered after a public tag exists.
#
# Usage:
#   scripts/build-release.sh [dist-dir]     # default: <repo>/dist
#
# Environment:
#   VERSION          version to stamp. Default: GITHUB_REF_NAME without its
#                    leading "v", else `git describe --tags --always --dirty`.
#   COMMIT           commit to stamp. Default: `git rev-parse HEAD`.
#   GITHUB_REF_NAME  the tag Actions is building, e.g. v1.2.3.
#
# Two things this deliberately does not do:
#
#   * It never post-processes the binaries. `strip` or UPX after linking breaks
#     the ad-hoc LC_CODE_SIGNATURE the Go linker emits for darwin/arm64, and the
#     result is SIGKILLed on Apple Silicon with no diagnostic. `-s -w` at link
#     time is safe and the signature survives it.
#   * It never passes -buildvcs=false. `go build` refuses to run inside a git
#     repository when it cannot exec git ("error obtaining VCS status"), and the
#     tempting fix also kills the debug.ReadBuildInfo fallback that gives
#     `go install`-built binaries a commit. A slim build container needs git
#     installed instead.

set -euo pipefail

# The sums file is consumed by an anchored regex in both installers, and the
# glob below decides its line order. Pin the collation so the file does not
# depend on the builder's locale.
export LC_ALL=C

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)

dist=${1:-$repo_root/dist}
mkdir -p -- "$dist"
dist=$(CDPATH= cd -- "$dist" && pwd)

version=${VERSION:-}
if [ -z "$version" ]; then
	ref=${GITHUB_REF_NAME:-}
	if [ -n "$ref" ]; then
		version=$ref
	else
		version=$(git -C "$repo_root" describe --tags --always --dirty 2>/dev/null || true)
	fi
	version=${version#v}
fi
if [ -z "$version" ]; then
	echo "build-release: cannot determine a version to stamp; set VERSION" >&2
	exit 1
fi

# An unstamped Commit is not a cosmetic loss: buildinfo.String() falls through
# to debug.ReadBuildInfo(), which reports the commit of whatever tree the
# builder happened to be in. Fail rather than ship a binary that misreports
# where it came from.
commit=${COMMIT:-$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || true)}
if [ -z "$commit" ]; then
	echo "build-release: cannot determine a commit to stamp; set COMMIT" >&2
	exit 1
fi

# The linker accepts an unmatched -X without a warning, so a wrong symbol path
# here fails silently and the binary reports "dev". The package path must be
# the full import path of internal/buildinfo, not main.
stamp=github.com/Kweiza/ccdaddy/internal/buildinfo
ldflags="-s -w -X ${stamp}.Version=${version} -X ${stamp}.Commit=${commit}"

targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

cd -- "$repo_root"

# The notice files ship WITH the binaries, not only in the repository.
# BSD-3-Clause and Apache-2.0 both require their notices to accompany a BINARY
# distribution, and a GitHub release is a binary distribution: somebody who
# downloads ccdad-linux-amd64 and never clones the repository has still received
# eight modules' worth of licensed code. They are hashed into sha256sums.txt
# with everything else, so they are covered by the same checksum and the same
# provenance attestation as the binaries.
notices="LICENSE NOTICE THIRD-PARTY-LICENSES.txt"

# Sweep before building, not after, so the glob below sees exactly what this
# run produced. An asset left over from an earlier run with a different target
# list would otherwise be hashed into the sums file and then attested and
# published, with nothing in the release that explains where it came from.
rm -f -- "$dist"/ccdad-* "$dist"/sha256sums.txt
for notice in $notices; do
	rm -f -- "$dist/$notice"
done

failed=
for target in $targets; do
	goos=${target%/*}
	goarch=${target#*/}
	ext=
	if [ "$goos" = windows ]; then
		ext=.exe
	fi
	asset=ccdad-$goos-$goarch$ext
	echo "build-release: $asset ($version)" >&2
	if ! GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 \
		go build -trimpath -ldflags "$ldflags" -o "$dist/$asset" ./cmd/ccdad; then
		failed="$failed $target"
	fi
done

for notice in $notices; do
	[ -f "$repo_root/$notice" ] || {
		echo "build-release: $notice is missing from the repository" >&2
		exit 1
	}
	cp -- "$repo_root/$notice" "$dist/$notice"
done

# Glob what actually landed rather than replaying the target list: a partial
# build then still gets a sums file that matches its own assets, instead of one
# naming a binary nobody can download.
cd -- "$dist"
shopt -s nullglob
assets=(ccdad-*)
shopt -u nullglob
if [ ${#assets[@]} -eq 0 ]; then
	echo "build-release: no assets in $dist" >&2
	exit 1
fi
# The binaries first, then the notices, so the file the installers parse opens
# with the lines they are looking for. Both installers match their own asset
# with an anchored per-line regex, so the extra lines are inert to them --
# but the shape check they run first ("does this look like a checksum file")
# would fail on a file that led with something else.
sums_inputs=("${assets[@]}")
for notice in $notices; do
	sums_inputs+=("$notice")
done
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum -- "${sums_inputs[@]}" >sha256sums.txt
else
	shasum -a 256 -- "${sums_inputs[@]}" >sha256sums.txt
fi

echo "build-release: ${#assets[@]} asset(s), ${#sums_inputs[@]} sums line(s) in $dist" >&2

if [ -n "$failed" ]; then
	echo "build-release: failed for:$failed" >&2
	exit 1
fi
