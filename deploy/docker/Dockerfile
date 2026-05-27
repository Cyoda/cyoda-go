# syntax=docker/dockerfile:1

# Stage a pre-chowned data directory. Distroless/static has no shell,
# so we can't `RUN mkdir && chown` in the final image. This two-stage
# pattern stages an empty /data owned by 65532:65532 in a temporary
# image, then COPY-chowns it into the distroless image. The resulting
# /var/lib/cyoda is then present with correct ownership, which Docker
# propagates to an empty named volume mounted on top of it on first
# container start.
#
# busybox pinned to a specific version for release-pipeline
# reproducibility. Bump intentionally; the stage is discarded post-build
# so image size is irrelevant.
FROM busybox:1.38 AS stage
RUN mkdir -p /data && chown 65532:65532 /data

# distroless/static pinned by digest for supply-chain integrity. The digest
# resolves to a multi-arch OCI image index (linux/amd64 + linux/arm64), so
# the COPY $TARGETPLATFORM/cyoda below still resolves under buildx.
# Refresh monthly via scripts/refresh-distroless-digest.sh.
FROM gcr.io/distroless/static@sha256:3592aa8171c77482f62bbc4164e6a2d141c6122554ace66e5cc910cadb961ff0
# was :latest, pinned 2026-04-27
ARG TARGETPLATFORM

# Binary path /cyoda is referenced by:
#   - deploy/docker/compose.yaml healthcheck ("/cyoda health")
#   - the ENTRYPOINT below
# Keep these in sync if the binary ever moves.
COPY $TARGETPLATFORM/cyoda /cyoda
COPY --from=stage --chown=65532:65532 /data /var/lib/cyoda

USER 65532:65532
EXPOSE 8080 9090 9091

ENTRYPOINT ["/cyoda"]
