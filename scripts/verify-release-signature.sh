#!/usr/bin/env bash
#
# Verify a published sha256sums.txt against its minisign signature, and check
# that the signature was issued for the release being asked about.
#
# Usage:
#   scripts/verify-release-signature.sh TAG PUBKEY SUMS SIG
#
# minisign is taken from PATH and is expected to be the upstream tool, not
# anything from this repository. That is the point of the exercise: this
# project's own signer and its own verifier are tested against each other, and
# a signer and a verifier wrong in the same way pass every round trip between
# them. Only a tool nobody here wrote can say the published artifact is good.
#
# This lives in a script rather than inside the workflow's `run:` block because
# the rule below is subtle enough to have a test, and a gate that lives only in
# a workflow file is worth nothing -- scripts/ci.sh says the same about its own
# checks, and this one runs on published releases only.

set -euo pipefail

if [ "$#" -ne 4 ]; then
	echo "usage: verify-release-signature.sh TAG PUBKEY SUMS SIG" >&2
	exit 2
fi

tag=$1
pubkey=$2
sums=$3
sig=$4

if [ -z "$tag" ]; then
	# An empty tag is refused rather than treated as "any release". As a skip
	# it would silently switch off the only check binding a genuine
	# (sums, signature) pair to the release it was published as, and the
	# caller most likely to pass one is a workflow whose upstream step
	# produced nothing.
	echo "verify-release-signature: no release was named" >&2
	exit 2
fi

# Two ed25519 checks happen in here, not one: minisign covers the file, and
# then covers the trusted comment separately. Until this returns, the trusted
# comment is text whoever served the file chose.
verified=$(minisign -V -p "$pubkey" -m "$sums" -x "$sig")

# minisign answers "is this signature authentic". It does not answer "is this
# the release I asked for", and the two come apart in the case this whole
# script exists for: an origin that chooses what to serve can hand back an
# OLDER release's genuine, correctly signed pair, with every signature check
# passing. sha256sums.txt carries no version of its own, so nothing else in the
# artifact can tell them apart.
comment=$(printf '%s\n' "$verified" | sed -n 's/^Trusted comment: //p')
if [ -z "$comment" ]; then
	echo "verify-release-signature: minisign verified $sums and printed no trusted comment, so nothing names the release" >&2
	exit 1
fi

# EXACTLY ONE tab-separated field equal to ccdaddy:<tag>, which is the rule
# internal/relsign enforces on the same comment; the two must not disagree
# about what a valid release is.
#
# Not a substring test. `ccdaddy:v1.2.3` is a substring of `ccdaddy:v1.2.30`,
# which is a real release one patch series away, and both are correctly signed
# by the same key -- so a substring test accepts a downgrade that the field
# test refuses. Measured against minisign 0.11 and this repository's own
# fixtures, not reasoned about.
#
# -F because the tag is data: read as a pattern, the dots in every tag this
# project cuts are wildcards. install.ps1 escapes for the same reason.
count=$(printf '%s' "$comment" | tr '\t' '\n' | grep -Fxc -- "ccdaddy:$tag" || true)
if [ "$count" != 1 ]; then
	printf 'verify-release-signature: the trusted comment (%s) does not name %s exactly once\n' "$comment" "$tag" >&2
	exit 1
fi

printf 'verify-release-signature: %s verifies against %s and names %s\n' "$sums" "$pubkey" "$tag"
