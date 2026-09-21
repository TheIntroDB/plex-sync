# tidb-plex container image.
#
# Two stages: a builder with the Go toolchain, and a runtime image with nothing
# in it but the binary and a CA bundle. The driver is pure Go, so no CGO and no
# libc are needed, which is what makes this small and portable.

FROM golang:1.27-alpine AS builder

WORKDIR /src

# Dependencies first, so a source change does not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "-s -w \
            -X github.com/TheIntroDB/plex-integration/internal/buildinfo.Version=${VERSION} \
            -X github.com/TheIntroDB/plex-integration/internal/buildinfo.Commit=${COMMIT} \
            -X github.com/TheIntroDB/plex-integration/internal/buildinfo.Date=${DATE}" \
        -o /out/tidb-plex .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/tidb-plex /tidb-plex

# The ledger, backups, undo journals and fingerprints live here, so mount it.
VOLUME ["/state"]
ENV TIDB_PLEX_STATE_DIR=/state

# When the container runs itself. 07:30 local, after Plex's own maintenance
# window, so the two are not writing to the same database at the same time.
ENV TIDB_PLEX_SCHEDULE="30 7 * * *"

# The schedule is held by this process, so the image needs no cron daemon, no
# shell and no second process. Without --yes a scheduled run reports what is
# missing and changes nothing; add it, as the Unraid template does, when you
# want the container to write.
ENTRYPOINT ["/tidb-plex"]
CMD ["schedule"]

# There is no shell in this image, so the check runs the binary itself.
HEALTHCHECK --interval=5m --timeout=20s --start-period=10s \
    CMD ["/tidb-plex", "version"]
