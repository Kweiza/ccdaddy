#!/usr/bin/env bash
#
# Refuse a release whose commit CI has never proved green.
#
# Usage: scripts/require-green-ci.sh <sha> [workflow-file]
#
# Environment:
#   GH_REPO | GITHUB_REPOSITORY  owner/repo (Actions sets the second)
#   GH_TOKEN                     what `gh api` authenticates with
#   CCDAD_CI_POLL                seconds between polls (default 15)
#   CCDAD_CI_GRACE               seconds to allow for a run to appear at all
#                                (default 120)
#   CCDAD_CI_TIMEOUT             seconds to wait for a running run to finish
#                                (default 1800)
#
# Exits 0 and names the run when the commit has a successful ci.yml run; exits
# 1 and says which check was missing otherwise.
#
# Why this exists: `.github/workflows/release.yml` builds and publishes six
# binaries plus an attestation for any tag matching v*, and the only test it
# ran was its own `go test` on ubuntu. A tag on a commit whose CI never ran, or
# ran red, published anyway.
#
# Four things this deliberately does not do:
#
#   * Read `repos/OWNER/REPO/commits/$SHA/check-runs`. Measured against the one
#     release this repository has published: that endpoint returns the RELEASE
#     workflow's own check runs for the same commit ("Gate the tag",
#     "Build and publish"). A gate that waits for every check run on the commit
#     therefore waits for the job it is running inside, forever. Asking
#     `actions/workflows/ci.yml/runs?head_sha=` names the workflow that has to
#     be green and cannot see itself.
#   * Wait on `workflow_run`. It does not fire usefully for a tag push, and a
#     tag is a different ref from the branch CI ran on. The check has to be by
#     commit, because the commit is the same object under both refs.
#   * Re-run the matrix here. The OS matrix is for tests and the release builds
#     on one runner; duplicating it doubles the slowest part of a release and
#     still proves nothing about the branch.
#   * Offer a human override for a RED run. A red CI plus a human in a hurry is
#     the exact combination the gate exists for, and an input that skips it is
#     an input someone will pass. The supported answer to "CI never ran on this
#     commit" is to run ci.yml against the ref with workflow_dispatch, which
#     still has to go green; the supported answer to "CI is red" is to fix it
#     and tag again.

set -euo pipefail

sha=${1:-}
workflow=${2:-ci.yml}
repo=${GH_REPO:-${GITHUB_REPOSITORY:-}}
poll=${CCDAD_CI_POLL:-15}
grace=${CCDAD_CI_GRACE:-120}
timeout=${CCDAD_CI_TIMEOUT:-1800}

if [ -z "$sha" ]; then
	echo "require-green-ci: no commit given" >&2
	echo "require-green-ci: usage: scripts/require-green-ci.sh <sha> [workflow-file]" >&2
	exit 1
fi
if [ -z "$repo" ]; then
	echo "require-green-ci: no repository; set GH_REPO or GITHUB_REPOSITORY" >&2
	exit 1
fi

errfile=$(mktemp)
trap 'rm -f -- "$errfile"' EXIT

query="repos/$repo/actions/workflows/$workflow/runs?head_sha=$sha&per_page=100"

echo "require-green-ci: waiting for a successful $workflow run on $sha" >&2

# SECONDS rather than date(1): one builtin, no subprocess per poll, and no
# dependence on a date implementation that differs between the runner and a
# developer's machine.
SECONDS=0
while :; do
	lines=
	if ! lines=$(gh api "$query" \
		--jq '.workflow_runs[] | "\(.status)\t\(.conclusion)\t\(.html_url)"' 2>"$errfile"); then
		# A 404 is not a transient failure, it is the wrong workflow file, and
		# waiting half an hour to say so helps nobody.
		if grep -q 'HTTP 404' "$errfile"; then
			echo "require-green-ci: no workflow $workflow in $repo" >&2
			cat "$errfile" >&2
			exit 1
		fi
		if [ "$SECONDS" -ge "$timeout" ]; then
			echo "require-green-ci: cannot reach the Actions API for $repo" >&2
			cat "$errfile" >&2
			exit 1
		fi
		cat "$errfile" >&2
		sleep "$poll"
		continue
	fi

	runs=0
	pending=0
	green=
	conclusions=
	while IFS=$'\t' read -r status conclusion url; do
		[ -n "$status" ] || continue
		runs=$((runs + 1))
		if [ "$status" != completed ]; then
			pending=$((pending + 1))
			continue
		fi
		if [ "$conclusion" = success ]; then
			green=$url
			break
		fi
		conclusions="$conclusions $conclusion"
	done <<EOF
$lines
EOF

	if [ -n "$green" ]; then
		echo "require-green-ci: $workflow is green on $sha — $green" >&2
		exit 0
	fi

	if [ "$runs" -eq 0 ]; then
		# The honest failure the whole grace period exists to distinguish from
		# a slow start: nothing will ever arrive, so say so rather than time
		# out in half an hour with a message about waiting.
		if [ "$SECONDS" -ge "$grace" ]; then
			echo "require-green-ci: $workflow has never run on $sha" >&2
			echo "require-green-ci: tag a commit that was pushed to a branch, or run $workflow against this ref with workflow_dispatch" >&2
			exit 1
		fi
	elif [ "$pending" -eq 0 ]; then
		# Every run for this commit finished and none of them succeeded.
		# Waiting cannot change that; a re-run would create a new run and this
		# job is not the thing that should ask for one.
		echo "require-green-ci: $workflow finished on $sha without succeeding:$conclusions" >&2
		echo "require-green-ci: fix $workflow on this commit and tag again" >&2
		exit 1
	elif [ "$SECONDS" -ge "$timeout" ]; then
		echo "require-green-ci: $workflow is still running on $sha after ${timeout}s ($pending run(s) in flight)" >&2
		exit 1
	fi

	sleep "$poll"
done
