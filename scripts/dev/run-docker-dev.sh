#!/bin/bash
# Dev helper: builds cyoda from source, produces a local image, runs compose.
# Contributors use this to test local changes in a container before they land.
#
# Build flags here (-ldflags="-s -w") are intentionally minimal — version
# injection and release optimizations are owned by .goreleaser.yaml. The
# :dev image tag makes it visually obvious this isn't a release build.
set -eu

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$PROJECT_ROOT"

# Detect host platform in dockers_v2 context form (linux/amd64 or linux/arm64).
case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "run-docker-dev.sh: unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
PLATFORM="linux/$ARCH"

# Stage binary under .buildctx/$PLATFORM/cyoda for the dockers_v2 context.
# NOTE: the root .dockerignore doesn't apply here — buildctx is the build
# context root for this invocation, not the repo root.
BUILDCTX="$PROJECT_ROOT/.buildctx"
trap 'rm -rf "$BUILDCTX"' EXIT
rm -rf "$BUILDCTX"
mkdir -p "$BUILDCTX/$PLATFORM"

echo "Building cyoda for $PLATFORM..."
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
    go build -ldflags="-s -w" -o "$BUILDCTX/$PLATFORM/cyoda" ./cmd/cyoda

LOCAL_TAG="ghcr.io/cyoda/cyoda:dev"
echo "Building image $LOCAL_TAG..."
# BuildKit auto-injects TARGETPLATFORM from --platform, so no --build-arg
# needed for the Dockerfile's `COPY $TARGETPLATFORM/cyoda /cyoda` line.
docker buildx build --load \
    --platform "$PLATFORM" \
    -t "$LOCAL_TAG" \
    -f deploy/docker/Dockerfile \
    "$BUILDCTX"

echo "Running compose..."
CYODA_IMAGE="$LOCAL_TAG" docker compose -f deploy/docker/compose.yaml up "$@"
