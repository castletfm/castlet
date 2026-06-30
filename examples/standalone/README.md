# Standalone example

Run Castlet end-to-end with everything wired up by one script:

- **Single sign-on** against a throwaway, in-memory [Dex](https://dexidp.io)
  OIDC provider — no Google account or Cloud Console needed.
- **Object-storage media** in a local [MinIO](https://min.io) (S3-compatible)
  container — uploaded media is stored in MinIO and served straight from it via
  presigned URLs, exercising the `s3` blob store.
- **Automatic transcription** with [whisper.cpp](https://github.com/ggerganov/whisper.cpp)
  running in a container — uploaded episodes are transcribed with no extra step.

Everything runs on `127.0.0.1`. Castlet data is throwaway; the whisper model is
cached under `.whisper/` for reuse, and MinIO data persists under `.minio/`.

## Requirements

- **Go** — to run Castlet from source
- **Docker or Podman** — runs the Dex, MinIO, and whisper.cpp containers
- **ffmpeg** — downmixes uploaded audio to what whisper.cpp expects
- **jq** — adapts whisper.cpp output to Castlet's transcript schema
- **curl** — model download and discovery wait

## Run

```sh
./examples/standalone/run.sh
```

On first run it downloads the whisper model (`ggml-large-v3.bin`, ~3 GB, by
default — multilingual, so it transcribes any language including Japanese with
no extra configuration; see
[Choosing a model and language](#choosing-a-model-and-language) to use a smaller
one) and pulls the container images, then starts Dex and Castlet. The model is
cached under `.whisper/` and the download resumes if interrupted, so later runs
start immediately. Then:

1. Open <http://127.0.0.1:18080/login> → **Log in with single sign-on**
2. Sign in to Dex as **`user@example.com`** / **`password`**
3. Create a channel (pick its **primary language** — it pre-selects the upload
   default), then use the **Upload** button (top bar) to add an episode
4. On the upload form, set the episode's **Spoken language** (defaults to the
   channel's; pick `Auto-detect` only if it varies). This drives whisper's
   language and a matching **punctuation** prompt.
5. Watch the transcript status on the episode list go
   **pending → processing → done** — whisper.cpp transcribed it automatically,
   with punctuation

Changed the model or transcriber after uploading? Use the **Re-transcribe**
button on the episode list to re-run it on the existing audio — no re-upload.

`Ctrl-C` stops Castlet and removes the Dex and MinIO containers.

## Files

| File | Purpose |
|------|---------|
| `run.sh` | Brings up MinIO + Dex + Castlet with transcription; cleans up on exit |
| `dex.yaml` | Dex config: a static `castlet` client and one test user |
| `transcribe.sh` | The `command` transcriber: ffmpeg → whisper.cpp → jq |

`run.sh` generates `.blobstore.json` (the `s3` blob store config pointing at the
local MinIO) at startup; it and the `.minio/` data dir are git-ignored.

## How transcription is wired

Castlet runs with `-transcriber command -transcribe-command transcribe.sh`. On
upload it enqueues a job; a background worker invokes `transcribe.sh` with the
audio path. That script converts the audio to 16 kHz mono WAV (ffmpeg), runs
`whisper-cli` in the whisper.cpp container, and uses `jq` to emit the JSON
Castlet expects:

```json
{"language":"en","segments":[{"start":0.0,"end":3.2,"text":"..."}]}
```

### Language and punctuation

Each episode's **spoken language is set on the upload form** (defaulting to the
channel's primary language). Castlet passes it to the transcriber, which forces
whisper to that language and adds a matching-language **punctuation prompt** — so
Japanese comes out as `今日は良い天気ですね。散歩に行きましょう。` rather than
`今日はいい天気ですね散歩に行きましょう`. Whisper drops punctuation on some audio
(especially Japanese) without that prompt, and a wrong-language prompt would
mistranslate, which is why the language is explicit per episode. Choosing
`Auto-detect` skips both (correct language, but punctuation not guaranteed).

Transcripts are segmented **per sentence** rather than in whisper's default ~30s
windows, so each transcript line's timecode tracks the actual speech. `transcribe.sh`
runs whisper with `-ml 16` for fine-grained timing and merges the pieces at
sentence punctuation (with a pause/length fallback for unpunctuated audio).

### Choosing a model

The default `large-v3` (multilingual, ~3 GB) is the best quality. Override via
env to trade size/speed:

```sh
WHISPER_MODEL=small ./examples/standalone/run.sh     # ~466 MB, faster
WHISPER_MODEL=tiny.en ./examples/standalone/run.sh   # tiny, English only
```

- **`WHISPER_MODEL`** (default `large-v3`) — name of a file under
  <https://huggingface.co/ggerganov/whisper.cpp>. Names ending in `.en`
  (`tiny.en`, `base.en`, …) are **English-only**; any other name is
  **multilingual** (`tiny`, `base`, `small`, `medium`, `large-v3`,
  `large-v3-turbo`). Larger = better and slower.
- **`WHISPER_LANG`** — force a language for *all* episodes, overriding their
  per-episode setting (e.g. `WHISPER_LANG=ja`).
- **`WHISPER_PROMPT`** — override the auto-selected punctuation prompt; set empty
  (`WHISPER_PROMPT=` ) to disable it.
- **`WHISPER_MODEL_URL`** — override the download URL for a model not in the
  ggerganov mirror (e.g. a fine-tuned Japanese model). The file is still loaded
  as `ggml-$WHISPER_MODEL.bin`, so set `WHISPER_MODEL` to a matching name.

`WHISPER_IMAGE` and `DEX_IMAGE` are likewise overridable. The first run with a
large model downloads several GB; it is cached under `.whisper/` afterward.

## Point it at a real OIDC provider instead

Drop the Dex container and pass your provider's issuer plus a registered client:

```sh
go run ./cmd/castlet serve \
  -addr 127.0.0.1:18080 -base-url http://127.0.0.1:18080 \
  -oidc-issuer https://accounts.google.com \
  -oidc-client-id "$CLIENT_ID" \
  -oidc-client-secret "$CLIENT_SECRET"
```

Register `http://127.0.0.1:18080/auth/oidc/callback` as an authorized redirect
URI with the provider. (Configuration also reads `CASTLET_OIDC_*` env vars.)
