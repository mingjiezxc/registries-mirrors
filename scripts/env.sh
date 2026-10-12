#!/usr/bin/env bash
# Source this file (or use the Makefile) before running any go command.
#
# Why: /root/go/pkg/mod is mounted read-only in this sandbox, so the Go module
# and build caches are relocated inside the repository. Nothing here is needed
# when building inside Docker (the Dockerfile uses the stock cache layout).
#
#   source scripts/env.sh && go build ./...

_RMS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Docker's client and buildx state live under $DOCKER_CONFIG. /root/.docker is
# read-only in this sandbox, so both are relocated here. This also keeps
# `docker login` credentials for the local end-to-end tests inside the repo.
export DOCKER_CONFIG="${DOCKER_CONFIG:-${_RMS_ROOT}/.docker}"
mkdir -p "${DOCKER_CONFIG}"

# GOPATH must move too, not just GOMODCACHE: the checksum database cache lives
# at $GOPATH/pkg/sumdb and would otherwise still write to the read-only /root/go.
export GOPATH="${_RMS_ROOT}/.gocache/gopath"
export GOMODCACHE="${GOPATH}/pkg/mod"
export GOCACHE="${_RMS_ROOT}/.gocache/build"

# Outbound HTTP(S) proxy. Required on 192.168.188.101 for module downloads and
# for reaching public registries; harmless on 192.168.188.103.
export HTTP_PROXY="${HTTP_PROXY:-http://192.168.188.1:3128}"
export HTTPS_PROXY="${HTTPS_PROXY:-http://192.168.188.1:3128}"
# Never proxy loopback, the LAN, or the SASE-reachable corporate ranges.
export NO_PROXY="${NO_PROXY:-localhost,127.0.0.1,192.168.188.0/24,10.82.0.0/16,10.44.0.0/16}"

mkdir -p "${GOMODCACHE}" "${GOCACHE}"
