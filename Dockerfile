# Minimal release image for `agentenv` — distroless static base, no shell, no
# package manager, just the binary. Built and pushed to ghcr.io by goreleaser
# (see `dockers:` in .goreleaser.yaml).
#
# The build context is goreleaser's per-arch dist/ subdirectory, which already
# contains the pre-compiled `agentenv` binary — so this Dockerfile is just a
# COPY + ENTRYPOINT. No `go build` happens here.
#
# Note: agentenv calls `bash -lc` inside the *managed rootfs* (work/current),
# never inside this image. So a distroless host image is fine; bash is only
# required in whatever rootfs you `init --from` or `init --tarball` later.
FROM gcr.io/distroless/static-debian12:nonroot

COPY agentenv /usr/local/bin/agentenv

ENTRYPOINT ["/usr/local/bin/agentenv"]
