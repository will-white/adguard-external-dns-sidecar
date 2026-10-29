# Devcontainer base image.
#
# 1-1.24 is the newest Go devcontainer image upstream publishes, while go.mod
# targets a newer Go. See GOTOOLCHAIN below for how that gap is closed.
FROM mcr.microsoft.com/devcontainers/go:2-1.24-bookworm

# The image hard-sets GOTOOLCHAIN=local, so a go.mod requiring a newer Go fails
# outright:
#   go: go.mod requires go >= 1.26 (running go 1.24.5; GOTOOLCHAIN=local)
# `auto` lets the go command fetch exactly the toolchain go.mod asks for, which
# keeps go.mod the single source of truth instead of pinning a Go version here
# (a pin would be a fourth version site, and Renovate cannot bump a feature
# option or an ENV).
ENV GOTOOLCHAIN=auto

# The upstream image ships /etc/apt/sources.list.d/yarn.list, but its signing
# key is not in the image keyring:
#   NO_PUBKEY 62D54FD4003F6525
# The repo itself is reachable, so this is a key problem, not a network one.
# Any feature that runs `apt-get update` (docker-in-docker, claude-code) then
# fails with exit 100 and the container cannot be built at all. Features run at
# image-build time, before every lifecycle command, so the fix has to happen
# here rather than in postCreateCommand.
#
# Nothing in this repo needs yarn - Node arrives via the node feature - so drop
# the source. Re-check on image bumps: if upstream fixes the key, this RUN can go.
RUN rm -f /etc/apt/sources.list.d/yarn.list
