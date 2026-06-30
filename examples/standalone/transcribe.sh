#!/usr/bin/env bash
# Castlet `command` transcriber backed by whisper.cpp running in a container.
#
# Castlet invokes this with the path to the uploaded audio file as $1 (the
# {{audio}} token). It must print ONLY the JSON schema Castlet expects on
# stdout:
#
#   {"language":"en","segments":[{"start":0.0,"end":3.2,"text":"..."}]}
#
# Pipeline: ffmpeg (host) downmixes to 16 kHz mono WAV -> whisper.cpp (container)
# transcribes to its own JSON -> jq adapts that to Castlet's schema.
#
# The episode's language arrives as $CASTLET_LANGUAGE (set by Castlet from the
# upload form). When it names a language, we force whisper's -l to it and add a
# matching-language punctuation prompt — whisper drops punctuation on some audio
# (especially Japanese) without one, and a wrong-language prompt would mistranslate.
# An empty value means auto-detect (no forced language, no prompt).
#
# Env overrides:
#   WHISPER_MODEL   ggml model name without the "ggml-"/".bin" (default large-v3,
#                   multilingual). Any model without the ".en" suffix handles
#                   non-English audio such as Japanese.
#   WHISPER_LANG    force a language, overriding the episode's (e.g. ja).
#   WHISPER_PROMPT  override the auto-selected punctuation prompt. Set empty to
#                   disable it.
#   WHISPER_IMAGE   whisper.cpp image (default ghcr.io/ggerganov/whisper.cpp:main)
set -euo pipefail

# default_prompt: a short punctuated sentence in the given language, used to
# coax whisper into emitting punctuation. Empty for unknown/auto.
default_prompt() {
  case "${1%%-*}" in
    ja) printf 'こんにちは。今日は、良い天気ですね。' ;;
    zh) printf '你好。今天天气很好，对吧？' ;;
    ko) printf '안녕하세요. 오늘은 날씨가 좋네요.' ;;
    auto | "") printf '' ;;
    *) printf 'Hello. Here is a sentence, with proper punctuation.' ;;
  esac
}

AUDIO="${1:?usage: transcribe.sh <audio-file>}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODELS="$HERE/.whisper/models"
MODEL="${WHISPER_MODEL:-large-v3}"
LANG_HINT="${WHISPER_LANG:-${CASTLET_LANGUAGE:-auto}}"
PROMPT="${WHISPER_PROMPT-$(default_prompt "$LANG_HINT")}"
IMAGE="${WHISPER_IMAGE:-ghcr.io/ggerganov/whisper.cpp:main}"

if [ ! -s "$MODELS/ggml-$MODEL.bin" ]; then
  echo "transcribe.sh: model $MODELS/ggml-$MODEL.bin missing (run.sh downloads it)" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# whisper.cpp wants 16 kHz mono 16-bit PCM WAV regardless of the upload format.
ffmpeg -nostdin -loglevel error -y -i "$AUDIO" -ar 16000 -ac 1 -c:a pcm_s16le "$work/audio.wav"

# Optional initial prompt, single-quoted for the inner shell (escaping any
# embedded single quotes) so spaces and non-ASCII characters pass through safely.
prompt_arg=""
if [ -n "$PROMPT" ]; then
  esc=$(printf '%s' "$PROMPT" | sed "s/'/'\\\\''/g")
  prompt_arg="--prompt '$esc'"
fi

# Transcribe inside the container. Keep our stdout clean JSON; send whisper's
# own logs (detected language, timings, warnings) to a file in the workdir so we
# can both archive them for review and surface them to Castlet if it fails.
# -oj writes /audio/out.json into the mounted workdir.
wlog="$HERE/.whisper/whisper.log"
set +e
docker run --rm \
  -v "$MODELS:/models:ro,Z" \
  -v "$work:/audio:Z" \
  "$IMAGE" \
  "/app/build/bin/whisper-cli -m /models/ggml-$MODEL.bin -l $LANG_HINT $prompt_arg -ml 16 -f /audio/audio.wav -oj -of /audio/out >/dev/null 2>/audio/whisper.err"
rc=$?
set -e
# Archive this run's whisper log (newest last) for later review.
{ echo "=== $(basename "$AUDIO") model=$MODEL lang=$LANG_HINT prompt=${PROMPT:+set} ==="
  cat "$work/whisper.err" 2>/dev/null; } >>"$wlog" 2>/dev/null || true
if [ "$rc" -ne 0 ]; then
  # Surface whisper's error to Castlet (it includes our stderr in its log).
  cat "$work/whisper.err" >&2 2>/dev/null || true
  echo "transcribe.sh: whisper-cli failed (rc=$rc); full log at $wlog" >&2
  exit "$rc"
fi

# Adapt whisper.cpp JSON to Castlet's schema, with natural sentence-level
# timecodes instead of fixed 30s windows. whisper is run with -ml 16, so each
# entry is a short, accurately-timed character run; we split each run at sentence
# punctuation and merge runs into one segment per sentence. A new segment also
# starts after a >700ms pause or once a run exceeds 12s (a cap for unpunctuated
# speech). Offsets are ms; Castlet wants seconds.
jq '
  .result.language as $lang
  | ([.transcription[] | {f: .offsets.from, t: .offsets.to,
       parts: (.text | [scan("[^。．.!！?？…]*[。．.!！?？…]|[^。．.!！?？…]+")])}]
     | reduce .[] as $c ({segs: [], cur: null};
         reduce $c.parts[] as $p (.;
           (if (.cur == null) or (($c.f - .cur.end) > 700) or ((.cur.end - .cur.start) > 12000)
            then (if .cur != null then .segs += [.cur] else . end) | .cur = {start: $c.f, end: $c.t, text: $p}
            else .cur.end = $c.t | .cur.text += $p end)
           | if ($p | test("[。．.!！?？…]$")) then .segs += [.cur] | .cur = null else . end))
     | .segs + (if .cur then [.cur] else [] end)) as $s
  | {language: $lang,
     segments: ($s | map({start: (.start / 1000), end: (.end / 1000), text: (.text | gsub("^\\s+|\\s+$"; ""))}))}
' "$work/out.json"
