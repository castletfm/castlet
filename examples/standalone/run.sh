#!/usr/bin/env bash
# Standalone Castlet demo: brings up a local Dex OIDC provider and Castlet wired
# to it, with automatic whisper.cpp transcription, in one command.
#
#   ./examples/standalone/run.sh
#
# - Log in:   http://127.0.0.1:18080/login  ->  single sign-on
#             Dex test user:  user@example.com / password
# - Upload an episode; whisper.cpp transcribes it automatically (status goes
#   pending -> processing -> done on the episode list).
#
# Media is stored in a local MinIO (S3-compatible) container and served straight
# from it via presigned URLs, demonstrating the s3 blob store.
#
# Ctrl-C stops Castlet and tears down the Dex and MinIO containers. Data persists
# between runs: Castlet's database in .data/, media in .minio/, Dex's storage in
# .dex/, and the whisper model cache in .whisper/. Delete those dirs to start clean.
#
# Requirements: docker (or podman), ffmpeg, jq, curl, go.
# Each episode's spoken language (set on the upload form) drives transcription
# and punctuation. Env overrides: WHISPER_MODEL (default large-v3, multilingual,
# ~3GB), WHISPER_LANG (force one language for ALL episodes), WHISPER_PROMPT,
# WHISPER_MODEL_URL, WHISPER_IMAGE, DEX_IMAGE. Set WHISPER_MODEL=small for a
# faster, lighter download. See the README.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
DEX_IMAGE="${DEX_IMAGE:-ghcr.io/dexidp/dex:v2.41.1}"
WHISPER_IMAGE="${WHISPER_IMAGE:-ghcr.io/ggerganov/whisper.cpp:main}"
WHISPER_MODEL="${WHISPER_MODEL:-large-v3}"
# WHISPER_LANG is intentionally NOT defaulted/exported here: leaving it unset
# lets transcribe.sh fall back to each episode's CASTLET_LANGUAGE. A user who
# exports WHISPER_LANG (e.g. WHISPER_LANG=ja ./run.sh) still forces it globally.
# Where to fetch the model from. Defaults to the ggerganov ggml mirror keyed by
# WHISPER_MODEL; override for a fine-tuned model hosted elsewhere (the file is
# still loaded as ggml-$WHISPER_MODEL.bin).
WHISPER_MODEL_URL="${WHISPER_MODEL_URL:-https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-$WHISPER_MODEL.bin}"
DATA_DIR="${DATA_DIR:-$HERE/.data}"
DEX_DATA="$HERE/.dex"
MINIO_DATA="$HERE/.minio"
MINIO_IMAGE="${MINIO_IMAGE:-quay.io/minio/minio}"
MC_IMAGE="${MC_IMAGE:-quay.io/minio/mc}"
MINIO_USER=castletkey
MINIO_PASS=castletsecret
MINIO_BUCKET=castlet-media
BLOB_CONFIG="$HERE/.blobstore.json"
MODELS="$HERE/.whisper/models"
MODEL_FILE="$MODELS/ggml-$WHISPER_MODEL.bin"

cleanup() { docker rm -f castlet-dex castlet-minio >/dev/null 2>&1 || true; }
# Trap signals too, not just EXIT: when go run is interrupted, bash may be
# killed by the same SIGINT/SIGTERM before reaching the EXIT trap.
trap 'cleanup; exit 0' INT TERM
trap cleanup EXIT

cleanup                     # remove leftover containers from a prior run
# Persisted data dirs are bind-mounted so they survive restarts; we do NOT wipe
# them. Delete .data/, .dex/ and .minio/ by hand to start clean.
mkdir -p "$DATA_DIR" "$DEX_DATA" "$MINIO_DATA"

# Run the helper containers as the host user so they can write their
# bind-mounted data dirs. podman additionally needs keep-id to map the uid 1:1.
RUN_USER=(--user "$(id -u):$(id -g)")
if docker --version 2>/dev/null | grep -qi podman; then
  RUN_USER+=(--userns=keep-id)
fi

# Guard against the common trap: a globally-forced WHISPER_LANG that an
# English-only (.en) model can't satisfy (it would emit "(speaking in foreign
# language)"). Per-episode languages are checked at transcribe time, not here.
case "$WHISPER_MODEL:${WHISPER_LANG:-}" in
  *.en:en | *.en:auto | *.en:) ;;
  *.en:*)
    echo "ERROR: WHISPER_MODEL=$WHISPER_MODEL is English-only, but WHISPER_LANG=${WHISPER_LANG:-}." >&2
    echo "       Use a multilingual model (drop the .en suffix), e.g.:" >&2
    echo "       WHISPER_MODEL=large-v3 WHISPER_LANG=${WHISPER_LANG:-} $0" >&2
    exit 1
    ;;
esac

# --- whisper.cpp: download the model and pre-pull the image (cached) ---------
mkdir -p "$MODELS"

# model_complete: true if the cached model exists and matches the server's
# size, so a partial/truncated file is re-downloaded rather than used. If the
# size can't be fetched (e.g. offline), trust the cached file.
model_complete() {
  [ -s "$MODEL_FILE" ] || return 1
  local remote local_size
  remote=$(curl -fsSLI "$WHISPER_MODEL_URL" 2>/dev/null | tr -d '\r' \
    | awk 'tolower($1)=="content-length:"{v=$2} END{print v}')
  [ -z "$remote" ] && return 0
  local_size=$(stat -c%s "$MODEL_FILE" 2>/dev/null || echo 0)
  [ "$remote" = "$local_size" ]
}

if ! model_complete; then
  echo "Downloading whisper model ggml-$WHISPER_MODEL.bin (one-time, resumable; large models are multi-GB) ..."
  # Resume into a .part file (so an interrupted download continues next run
  # instead of restarting), then move it into place only when complete.
  curl -fSL --retry 3 --retry-delay 2 -C - -o "$MODEL_FILE.part" "$WHISPER_MODEL_URL"
  mv -f "$MODEL_FILE.part" "$MODEL_FILE"
fi
echo "Pulling whisper.cpp image ($WHISPER_IMAGE) ..."
docker pull "$WHISPER_IMAGE" >/dev/null

# --- MinIO (S3-compatible media blob store) ----------------------------------
echo "Starting MinIO ($MINIO_IMAGE) ..."
docker run -d --name castlet-minio "${RUN_USER[@]}" \
  -p 127.0.0.1:9000:9000 \
  -e MINIO_ROOT_USER="$MINIO_USER" -e MINIO_ROOT_PASSWORD="$MINIO_PASS" \
  -v "$MINIO_DATA:/data:Z" \
  "$MINIO_IMAGE" server /data >/dev/null

echo -n "Waiting for MinIO "
for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null http://127.0.0.1:9000/minio/health/live 2>/dev/null; then
    echo "ready."
    break
  fi
  echo -n "."
  sleep 1
done

echo "Ensuring bucket $MINIO_BUCKET exists ..."
docker run --rm --network host -e MC_HOST_loc="http://$MINIO_USER:$MINIO_PASS@127.0.0.1:9000" \
  "$MC_IMAGE" mb --ignore-existing "loc/$MINIO_BUCKET" >/dev/null

# Point Castlet at MinIO. Castlet redirects /media/{key} to a presigned URL, so
# media is served straight from MinIO, not through Castlet.
cat >"$BLOB_CONFIG" <<JSON
{
  "type": "s3",
  "endpoint": "http://127.0.0.1:9000",
  "region": "us-east-1",
  "bucket": "$MINIO_BUCKET",
  "access_key": "$MINIO_USER",
  "secret_key": "$MINIO_PASS",
  "prefix": "media/"
}
JSON

# --- Dex ---------------------------------------------------------------------
echo "Starting Dex ($DEX_IMAGE) ..."
docker run -d --name castlet-dex "${RUN_USER[@]}" \
  -p 127.0.0.1:5556:5556 \
  -v "$HERE/dex.yaml:/etc/dex/config.yaml:ro,Z" \
  -v "$DEX_DATA:/var/dex:Z" \
  "$DEX_IMAGE" dex serve /etc/dex/config.yaml >/dev/null

echo -n "Waiting for Dex discovery "
for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null http://127.0.0.1:5556/dex/.well-known/openid-configuration 2>/dev/null; then
    echo "ready."
    break
  fi
  echo -n "."
  sleep 1
done

cat <<'BANNER'

Castlet is starting at http://127.0.0.1:18080
  1. open http://127.0.0.1:18080/login
  2. click "Log in with single sign-on"  (Dex user: user@example.com / password)
  3. create a channel, then use "Upload" to add an episode (stored in MinIO)
  4. whisper.cpp transcribes it automatically — watch the transcript status
Ctrl-C to stop everything.

BANNER

# Foreground (not exec) so the EXIT trap fires and Dex is cleaned up on Ctrl-C.
# Export the model/image so transcribe.sh sees them; the per-episode language
# reaches it via Castlet (as CASTLET_LANGUAGE), and a user-set WHISPER_LANG /
# WHISPER_PROMPT already propagates through the environment.
export WHISPER_MODEL WHISPER_IMAGE
cd "$REPO"
go run ./cmd/castlet serve \
  -addr 127.0.0.1:18080 -base-url http://127.0.0.1:18080 \
  -data-dir "$DATA_DIR" \
  -blob-store-config "$BLOB_CONFIG" \
  -oidc-issuer http://127.0.0.1:5556/dex \
  -oidc-client-id castlet \
  -oidc-client-secret castlet-secret \
  -transcriber command \
  -transcribe-command "$HERE/transcribe.sh"
