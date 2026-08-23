#!/usr/bin/env bash
#
# Assert that an installed ccdad reports the version its release claims to
# ship.
#
# Usage: scripts/smoke-version.sh v1.2.3 /path/to/ccdad
#
# Exits 0 and names the line it accepted; exits 1 and says which part of it was
# wrong otherwise. Everything goes to stderr - this script has no output to
# capture, only a verdict.
#
# This is the entire assertion of .github/workflows/install-smoke.yml, and it
# lives here rather than inside a `run:` block for the same reason
# scripts/tag-gate.sh does: a rule that can only be exercised by publishing a
# release is a rule nobody can fix safely. The workflow calls it once per
# install it performs - three fresh ones and both halves of the upgrade.
#
# `ccdad --version` is Cobra's version template, not a bare version. Measured
# on a release binary:
#
#     ccdad version 1.2.3 (c24609320a6a)
#
# so `[ "$(ccdad --version)" = "$TAG" ]` - the assertion anyone would write
# first - fails one hundred per cent of the time, and the commit suffix is
# always there because scripts/build-release.sh refuses to build without a
# commit to stamp. The WHOLE line is matched rather than a single awk field, so
# that a change to the template fails here by name instead of leaving a parser
# quietly reading the wrong word.
#
# The commit is asserted for its shape and never against a specific sha. What
# a released binary was built from is scripts/build_release_test.go's question,
# and it answers it by running the host-native asset it just built; by the time
# this script runs, the only thing it could compare against is a tag ref that
# says nothing the version field has not already said. Twelve lowercase hex
# digits is what buildinfo.String() truncates a stamped commit to, so a binary
# that answers anything else was linked without one.

set -euo pipefail

expected=${1:-}
binary=${2:-}

if [ -z "$expected" ] || [ -z "$binary" ]; then
	echo "smoke-version: usage: scripts/smoke-version.sh <tag-or-version> <path-to-ccdad>" >&2
	exit 1
fi

# Callers hold a tag; buildinfo holds a version. tag-gate.sh strips the "v"
# once for the release stamp and this strips it again for the comparison,
# rather than making every caller remember which of the two it has.
want=${expected#v}

if [ ! -x "$binary" ]; then
	echo "smoke-version: $binary is not an executable file — the installer did not leave one there" >&2
	exit 1
fi

# stdout only. The installers and the daemon both write their notes to stderr,
# and folding those in here would put a line in front of the one being matched.
if ! line=$("$binary" --version); then
	echo "smoke-version: $binary --version failed; a binary that will not run is not an installed binary" >&2
	exit 1
fi

# The first line, without the carriage return Git Bash leaves on the end of a
# Windows binary's output. An anchored regex matches neither of those, and the
# resulting failure would be about the template rather than about line endings.
line=${line%%$'\n'*}
line=${line//$'\r'/}

# Bash 3.2 on the macOS runner needs the pattern through a variable and
# unquoted; quoting it there makes =~ a literal string comparison.
template='^ccdad version ([^[:space:]]+) \(([0-9a-f]{12})\)$'
if ! [[ $line =~ $template ]]; then
	echo "smoke-version: $binary --version printed" >&2
	echo "smoke-version:     $line" >&2
	echo "smoke-version: expected Cobra's template — 'ccdad version <version> (<twelve hex digits>)'" >&2
	echo "smoke-version: either the template changed or the binary was linked without a stamp" >&2
	exit 1
fi

got=${BASH_REMATCH[1]}
if [ "$got" != "$want" ]; then
	echo "smoke-version: $binary reports version $got; the release says $want" >&2
	echo "smoke-version: \"dev\" here means the linker was given a symbol that does not exist" >&2
	exit 1
fi

echo "smoke-version: $line — version $want, as the release claims" >&2
