# A reference image, not a published one. Nothing publishes it: there is no GHCR
# job, and the release workflow under .github/workflows is untouched by this
# file. Build it yourself, from a binary you built yourself.
#
# node is the base because Claude Code is an npm package, and because the probe
# and the automatic mode both run `claude -p` from inside this container — an
# image without it would carry a ccdad that cannot do half of what it is for.
# 22 is the current active LTS line; the package needs 18 or newer.
FROM node:22-slim

RUN npm install -g @anthropic-ai/claude-code

# The binary is copied, not built here. A Go toolchain in a build stage would add
# several hundred megabytes to produce a file the repository already tells you
# how to make:
#   CGO_ENABLED=0 GOOS=linux go build -o ccdad ./cmd/ccdad
# /ccdad is the path .gitignore already reserves for exactly that output.
COPY ccdad /usr/local/bin/ccdad
COPY ccdad-entrypoint /usr/local/bin/ccdad-entrypoint

# COPY carries the source file's mode, and the source mode depends on how the
# file reached the build machine: a zip download and a Windows checkout both
# arrive without the executable bit. The container would then fail at start with
# a permission error rather than at build, which is the wrong end of the process
# to find out.
RUN chmod 0755 /usr/local/bin/ccdad /usr/local/bin/ccdad-entrypoint

# Two independent axes, both set. CCDAD_HOME moves ccdad's own store; it does NOT
# move the Claude Code login ccdad manages, which follows CLAUDE_CONFIG_DIR.
# Setting only the first leaves the login inside the image layer while the store
# sits on the volume — so a restart comes up with accounts and no login, and two
# containers sharing one volume run two engines over one login and undo each
# other's switches.
ENV CCDAD_HOME=/data/ccdad CLAUDE_CONFIG_DIR=/data/claude

# Without a volume here, every restart loses the account store, the usage cache
# and the anti-flap state, and re-imports from the mounted secret. The cache is
# the expensive half: the usage endpoint allows roughly 28-30 requests per
# identity per rolling hour, and a container that starts cold spends that budget
# again from zero.
VOLUME /data

ENTRYPOINT ["/usr/local/bin/ccdad-entrypoint"]

# `exec "$@"` with no arguments is a no-op, so a container started with no
# command would run the entrypoint and exit 0 the moment the shell returned,
# taking the daemon it had just started with it. A shell is what this image is
# for: the engine is running and `claude` is on PATH.
CMD ["sh"]
