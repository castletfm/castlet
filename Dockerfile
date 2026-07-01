# Castlet production container image for homelab / intranet self-hosting.
#
# Multi-stage build:
#   1. builder — compiles a static, CGO-free binary (modernc.org/sqlite is pure
#      Go, so no C toolchain is needed).
#   2. final   — distroless static base, runs as a non-root user.
#
# Build:
#   docker build -t castlet .
#   docker build --build-arg VERSION="$(git describe --tags --always)" -t castlet .
#
# Run (session key is required to survive restarts; generated per-run otherwise):
#   docker run --rm -p 8080:8080 \
#     -v castlet-data:/data \
#     -e CASTLET_SESSION_KEY="$(head -c32 /dev/urandom | base64)" \
#     -e CASTLET_BASE_URL="http://localhost:8080" \
#     castlet
#
# Transcription note:
#   This image ships ONLY the built-in "null" transcriber. The "command"
#   transcriber (whisper.cpp + ffmpeg) is intentionally NOT bundled to keep the
#   image minimal. Operators who want transcription should extend this image
#   (install ffmpeg/whisper.cpp on a suitable base) or mount the tools in and set
#   CASTLET_TRANSCRIBER=command with CASTLET_TRANSCRIBE_COMMAND/ARGS.

# ---- builder ----------------------------------------------------------------
# Base image tag tracks the Go version declared in go.mod (go 1.26).
FROM golang:1.26 AS builder

WORKDIR /src

# Cache module downloads separately from the source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version string stamped into main.version (cmd/castlet/main.go).
ARG VERSION=dev

# CGO disabled -> fully static binary (pure-Go sqlite driver).
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/castlet ./cmd/castlet

# Data dir owned by the non-root runtime user, so the default volume is writable
# even on distroless (which has no shell to mkdir/chown at runtime).
RUN mkdir -p /data

# ---- final ------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

# The distroless "nonroot" user is uid/gid 65532.
COPY --from=builder /out/castlet /usr/local/bin/castlet
COPY --from=builder --chown=65532:65532 /data /data

# Persist the sqlite database and media blobs here.
ENV CASTLET_DATA_DIR=/data
VOLUME ["/data"]

# Default listen address is :8080 (see config.go / CASTLET_ADDR).
EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/castlet"]
CMD ["serve"]
