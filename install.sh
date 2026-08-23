#!/usr/bin/env bash
#
# ccdad installer for macOS and Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/Kweiza/ccdaddy/main/install.sh | bash
#
# Environment:
#   CCDAD_INSTALL_DIR  where the binary goes (default: ~/.local/bin)
#   CCDAD_VERSION      a released tag to pin, e.g. v1.2.3 (default: the latest
#                      non-prerelease). Prereleases are published but are never
#                      "latest", so they can only be installed by pinning.
#   CCDAD_BASE_URL     download origin, for mirrors and for the test harness
#
# `curl | bash` puts the script itself on stdin, so nothing here may read from
# stdin: no prompts, no `read`, and therefore no offer to edit a shell profile.
# The PATH note at the end exists because of that, and it names `ccdad
# setup-path` -- the command whose whole job is this, which knows which startup
# files the user's shell actually reads and writes a marker-fenced block that a
# second run rewrites in place. It is named by ABSOLUTE path, because this arm
# fires precisely when the install directory is off PATH, so a bare `ccdad`
# would not resolve. The export line stays underneath it for the reader who
# wants their current shell fixed this second.
#
# This is the only thing verifying the download, so every failure to verify is
# an abort, never a warning. There are three distinct ones: the checksum file
# not arriving, the asset not being listed in it, and a mismatch.

set -euo pipefail

REPO_SLUG=Kweiza/ccdaddy
BASE_URL=${CCDAD_BASE_URL:-https://github.com/$REPO_SLUG/releases}
VERSION=${CCDAD_VERSION:-}

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

info() {
	printf '%s\n' "$*" >&2
}

if [ -n "${CCDAD_INSTALL_DIR:-}" ]; then
	INSTALL_DIR=$CCDAD_INSTALL_DIR
elif [ -n "${HOME:-}" ]; then
	INSTALL_DIR=$HOME/.local/bin
else
	die "HOME is not set; point CCDAD_INSTALL_DIR at an install directory"
fi

# ---------------------------------------------------------------- the target

# clauth's bug is accepting only x86_64 on Linux, which bounces every arm64
# user — and WSL correctly reports Linux, so it is a large group. Git Bash and
# MSYS report a Windows-shaped uname, where the right answer is a different
# installer rather than an unsupported-platform error.
os=$(uname -s)
arch=$(uname -m)
case "$os" in
Linux) goos=linux ;;
Darwin) goos=darwin ;;
MINGW* | MSYS* | CYGWIN* | Windows_NT)
	die "this is Windows ($os). Use install.ps1:
    irm https://raw.githubusercontent.com/$REPO_SLUG/main/install.ps1 | iex"
	;;
*) die "unsupported operating system: $os (ccdad ships macOS, Linux and Windows)" ;;
esac
case "$arch" in
x86_64 | amd64) goarch=amd64 ;;
aarch64 | arm64) goarch=arm64 ;;
*) die "unsupported architecture: $arch (ccdad ships amd64 and arm64)" ;;
esac
ASSET=ccdad-$goos-$goarch

if [ -n "$VERSION" ]; then
	tag=$VERSION
	case "$tag" in
	v*) ;;
	*) tag=v$tag ;;
	esac
	DOWNLOAD=$BASE_URL/download/$tag
else
	# Deliberately not api.github.com/repos/.../releases/latest: that is rate
	# limited to sixty requests an hour unauthenticated, which turns an install
	# behind a corporate NAT or inside CI into a mystery failure. This path
	# costs no API call, at the price of a redirect — hence the shape checks
	# below, which exist to catch a proxy answering with an HTML page.
	DOWNLOAD=$BASE_URL/latest/download
fi

# ------------------------------------------------------------- the downloader

# Pin the protocol so a redirect cannot downgrade the transfer, but only for
# the default origin: an explicit CCDAD_BASE_URL is a deliberate choice and may
# legitimately be plain HTTP on a mirror or a test server. Unquoted on purpose,
# so an empty value expands to no argument at all.
proto_opts=
if [ -z "${CCDAD_BASE_URL:-}" ]; then
	proto_opts="--proto =https --tlsv1.2"
fi

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL $proto_opts -o "$2" -- "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" -- "$1"; }
else
	die "neither curl nor wget is available"
fi

if command -v sha256sum >/dev/null 2>&1; then
	digest() { sha256sum -- "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	digest() { shasum -a 256 -- "$1" | cut -d' ' -f1; }
else
	die "neither sha256sum nor shasum is available — refusing to install unverified"
fi

# ------------------------------------------------------------------ download

mkdir -p -- "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"

# The scratch directory is INSIDE the install directory so that the final move
# is a same-filesystem rename. /tmp is a different filesystem on most distros,
# and a cross-device mv degrades to a copy, which is ETXTBSY over a running
# binary.
tmp=$(mktemp -d "$INSTALL_DIR/.ccdad-install.XXXXXX") || die "cannot create a scratch directory in $INSTALL_DIR"
trap 'rm -rf -- "$tmp"' EXIT

sums=$tmp/sha256sums.txt
fetch "$DOWNLOAD/sha256sums.txt" "$sums" ||
	die "cannot download $DOWNLOAD/sha256sums.txt — refusing to install unverified"
if ! grep -Eq '^[0-9a-f]{64}  ' "$sums"; then
	die "$DOWNLOAD/sha256sums.txt is not a checksum file — a proxy or an error page?"
fi

# Anchored at both ends and TWO spaces, which is exactly what sha256sum and
# macOS `shasum -a 256` emit. One space matches nothing; a missing trailing
# anchor lets ccdad-linux-amd64 match a ccdad-linux-amd64.exe-shaped neighbour.
expected=$(grep -E "^[0-9a-f]{64}  ${ASSET}\$" "$sums" | head -n 1 | cut -d' ' -f1 || true)
[ -n "$expected" ] || die "$ASSET is not listed in $DOWNLOAD/sha256sums.txt — refusing to install unverified"

info "downloading $ASSET"
fetch "$DOWNLOAD/$ASSET" "$tmp/$ASSET" || die "cannot download $DOWNLOAD/$ASSET"

size=$(wc -c <"$tmp/$ASSET" | tr -d ' ')
if [ "$size" -lt 1000000 ]; then
	die "$ASSET downloaded as $size bytes, which is not a ccdad binary — a proxy or an error page?"
fi

actual=$(digest "$tmp/$ASSET")
if [ "$actual" != "$expected" ]; then
	die "checksum mismatch for $ASSET
    expected $expected
    got      $actual"
fi

# ------------------------------------------------------------------- install

TARGET=$INSTALL_DIR/ccdad

# ccdad's daemon is self-managed and holds a singleton lock, so replacing the
# binary underneath it leaves the OLD daemon running old code indefinitely.
# This runs the OLD binary, which may predate the daemon command group — today
# it answers `unknown command "daemon"` and exits 2 — so a non-zero exit means
# "nothing to stop" and must never abort an upgrade.
if [ -x "$TARGET" ]; then
	"$TARGET" daemon stop >/dev/null 2>&1 || true
fi

chmod 0755 "$tmp/$ASSET"
mv -f -- "$tmp/$ASSET" "$TARGET"

info "installed $("$TARGET" --version 2>/dev/null || echo ccdad) to $TARGET"

case ":${PATH:-}:" in
*":$INSTALL_DIR:"*) ;;
*)
	info ""
	info "$INSTALL_DIR is not on your PATH. This installer never edits a shell"
	info "profile — it cannot ask, and guessing at a startup file is how"
	info "installers corrupt them. ccdad will do it once you ask it to:"
	info ""
	info "    \"$TARGET\" setup-path"
	info ""
	info "or, to fix just this shell right now:"
	info ""
	info "    export PATH=\"$INSTALL_DIR:\$PATH\""
	;;
esac

info ""
info "To remove ccdad later run 'ccdad uninstall', not 'rm $TARGET': there is a"
info "daemon to stop, a token directory to clear and possibly an MCP entry to unwire."
