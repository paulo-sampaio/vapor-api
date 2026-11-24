# syntax=docker/dockerfile:1

FROM golang:1.27-trixie AS builder

WORKDIR /src

# Copy Go module manifests first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy API source code
COPY . ./

# Build static CGO_ENABLED=0 binaries
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/vapor-api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/vapor-sync ./cmd/sync

# DB_SOURCE modes:
# - auto (default): use prebaked data/hardware.db if present, else build from seed_hardware.json
# - prebaked: require data/hardware.db (fail if missing)
# - seed: always build from offline seed_hardware.json
ARG DB_SOURCE="auto"
RUN mkdir -p /out/data /out/tmp && chmod 1777 /out/tmp
COPY data/ /tmp/prebaked-data/
RUN case "$DB_SOURCE" in \
      prebaked) \
        test -f "/tmp/prebaked-data/hardware.db" || { echo "DB_SOURCE=prebaked but data/hardware.db is missing" >&2; exit 1; }; \
        cp /tmp/prebaked-data/hardware.db /out/data/hardware.db ;; \
      seed) \
        /out/vapor-sync --seed ./testdata/seed_hardware.json --out /out/data/hardware.db ;; \
      auto|"") \
        if [ -f "/tmp/prebaked-data/hardware.db" ]; then \
          cp /tmp/prebaked-data/hardware.db /out/data/hardware.db; \
        else \
          /out/vapor-sync --seed ./testdata/seed_hardware.json --out /out/data/hardware.db; \
        fi ;; \
      *) \
        echo "Invalid DB_SOURCE: $DB_SOURCE" >&2; exit 1 ;; \
    esac && \
    chmod 0444 /out/data/hardware.db

# Create minimal non-root passwd/group entries (UID/GID 65532)
RUN printf 'vapor:x:65532:65532:vapor:/:/sbin/nologin\n' > /out/passwd && \
    printf 'vapor:x:65532:\n' > /out/group

# ── Runtime Stage: FROM scratch ──
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/passwd /etc/passwd
COPY --from=builder /out/group /etc/group
COPY --from=builder /out/vapor-api /vapor-api
COPY --from=builder /out/data/hardware.db /data/hardware.db
COPY --from=builder --chown=65532:65532 /out/tmp /tmp

USER 65532:65532

ENV VAPOR_ADDR=:8080 \
    VAPOR_DB_PATH=/data/hardware.db

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/vapor-api", "-healthcheck"]

ENTRYPOINT ["/vapor-api"]
