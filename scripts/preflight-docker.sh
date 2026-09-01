#!/usr/bin/env bash
# Pre-flight for the Docker-backed test tiers.
#
# The tiers that matter stand up real PostgreSQL containers. When Docker cannot
# serve them the tests fail in a way that reads like a product defect: a full
# Docker VM disk surfaces only as
#
#   start postgres container: ... wait until ready: container exited with code 1
#
# with the actual cause (`FATAL: could not write to file ...: No space left on
# device`) buried in a container log the test discards. Diagnosing that from a
# test failure has cost this project hours.
#
# So: check here, fail loudly, and say what to do. There is deliberately no
# Docker-free fallback tier — an unavailable Docker is a problem to surface,
# not to work around.
set -euo pipefail

PROBE_IMAGE="${PROBE_IMAGE:-postgres:17-alpine}"

fail() {
  echo "" >&2
  echo "PRE-FLIGHT FAILED: $1" >&2
  echo "" >&2
  shift
  for line in "$@"; do echo "  $line" >&2; done
  echo "" >&2
  echo "Do not work around this by running a narrower tier. Fix Docker, then re-run." >&2
  exit 1
}

# Resolve the docker binary by EXECUTING candidates, not by looking one up.
# Two things defeat a naive `command -v docker` here:
#   - a Docker Desktop install symlinks /usr/local/bin/docker into
#     /Applications, where macOS privacy protection can deny stat() to a
#     non-interactive shell, so lookup reports nothing for a working docker;
#   - `docker` may resolve only in an interactive shell's environment, while
#     make recipes run under sh and never see it.
# Export DOCKER_BIN to override.
resolve_docker() {
  for cand in "${DOCKER_BIN:-}" docker /usr/local/bin/docker \
              /Applications/Docker.app/Contents/Resources/bin/docker \
              "$HOME/.docker/bin/docker" /opt/homebrew/bin/docker; do
    [ -n "$cand" ] || continue
    # --version is client-only and succeeds with the daemon down, which is
    # what lets the daemon check below report that case precisely instead of
    # every candidate failing and the script blaming the binary.
    if "$cand" --version >/dev/null 2>&1; then
      echo "$cand"
      return 0
    fi
  done
  return 1
}

if ! DOCKER="$(resolve_docker)"; then
  fail "docker could not be executed." \
       "Either it is not installed, or the daemon is not responding." \
       "Start Docker Desktop and wait for it to report running, then re-run." \
       "If docker lives somewhere unusual, set DOCKER_BIN to its full path."
fi

# `docker version` can succeed against a live client with a dead daemon.
if ! "$DOCKER" info >/dev/null 2>&1; then
  fail "the Docker daemon is not responding." \
       "The client works, so Docker is installed but not running." \
       "Start Docker Desktop and wait for it to report running, then re-run."
fi

if ! "$DOCKER" image inspect "$PROBE_IMAGE" >/dev/null 2>&1; then
  fail "the probe image $PROBE_IMAGE is not present locally." \
       "Registry pulls have been unreliable on this machine, so the suites rely" \
       "on locally-cached images. Restore it before running the Docker tiers."
fi

# Prove the VM disk can actually take a write. `docker info` stays happy on a
# full disk; only a real write finds it. 64 MiB is far less than a PostgreSQL
# initdb needs, so passing here is a floor, not a guarantee.
if ! "$DOCKER" run --rm "$PROBE_IMAGE" \
     sh -c 'dd if=/dev/zero of=/tmp/.preflight bs=1M count=64 >/dev/null 2>&1 && rm -f /tmp/.preflight' \
     >/dev/null 2>&1; then
  fail "Docker cannot write 64 MiB inside a container — its VM disk is full." \
       "The host having free space is not the same thing; check the VM, not df." \
       "" \
       "Reclaim, in this order (none of these touch images, which cannot be re-pulled here):" \
       "  docker volume prune -f     # anonymous testcontainer volumes" \
       "  docker builder prune -f" \
       "  docker container prune -f --filter label=org.testcontainers=true" \
       "" \
       "Plain 'volume prune' spares named volumes. Do NOT add -a."
fi
