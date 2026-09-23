# plex-sync container image.
#
# The release pipeline (GoReleaser, dockers_v2) has already compiled the binary
# for every target platform with the version, commit and date baked in, so this
# Dockerfile does not build anything: it copies the finished binary out of the
# build context and adds the runtime bits. The driver is pure Go, so no CGO and
# no libc are needed, which is what makes the image small and portable.

FROM gcr.io/distroless/static-debian12:nonroot

# GoReleaser stages the binary per platform as $TARGETPLATFORM/plex-sync.
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/plex-sync /plex-sync

# The ledger, backups, undo journals and fingerprints live here, so mount it.
VOLUME ["/state"]
ENV PLEX_SYNC_STATE_DIR=/state

# When the container runs itself. 07:30 local, after Plex's own maintenance
# window, so the two are not writing to the same database at the same time.
ENV PLEX_SYNC_SCHEDULE="30 7 * * *"

# The schedule is held by this process, so the image needs no cron daemon, no
# shell and no second process. Without --yes a scheduled run reports what is
# missing and changes nothing; add it, as the Unraid template does, when you
# want the container to write.
ENTRYPOINT ["/plex-sync"]
CMD ["schedule"]

# There is no shell in this image, so the check runs the binary itself.
HEALTHCHECK --interval=5m --timeout=20s --start-period=10s \
    CMD ["/plex-sync", "version"]