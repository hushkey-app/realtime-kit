# Package-only image, same shape as Pack's. The static `realtime-kit` binary is
# cross-compiled by the CI `compile` step (plain `GOOS/GOARCH`, no qemu); here we
# only drop it into distroless, so the image build is a single COPY.
#
# The binary's arch must match the image platform, which CI sets via
# `docker build --platform` (linux/amd64 for prod/Vultr, linux/arm64 for dev).
#
# Runs beside Pack in the same compose stack. It binds all interfaces because a
# container's loopback is its own — never publish the port; keep it on the
# stack network only, since callers send LiveKit API secrets in request bodies.
FROM gcr.io/distroless/static-debian12:nonroot

COPY dist/realtime-kit /realtime-kit

ENV REALTIME_KIT_ADDR=:7890
EXPOSE 7890

ENTRYPOINT ["/realtime-kit"]
