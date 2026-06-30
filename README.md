# Castlet

A minimalist, self-hostable **podcast server** written in Go. It serves public
channel and episode pages (with synced transcripts), generates podcast RSS
feeds, provides an admin area to manage channels and upload episodes
(audio **or** video), and transcribes episodes asynchronously.

Castlet is **standalone by default** — a single binary with an embedded SQLite
database, local-filesystem media storage, and an in-process worker — but every
external dependency sits behind an interface, so you can swap in
"enterprise-y" backends (Postgres, object storage, a hosted transcriber, an
external job queue) without touching the application.

No community features: no comments, likes, or follows.

## Features

- Users, channels (many per user), and episodes
- Audio and video episodes, with the right player chosen automatically
- Server-rendered, crawlable public pages — no JavaScript build step
- Per-channel RSS 2.0 / iTunes feed
- Click-to-seek transcript synced to playback
- Asynchronous transcription worker (pluggable engine)
- Cookie-based admin authentication
- Single static binary; no external services required

## Quick start

```sh
# build
go build -o castlet ./cmd/castlet

# create the first admin user (creates ./data on first run)
./castlet user-create --email you@example.com --password 'choose-one' --name 'You'

# run the server + worker
CASTLET_SESSION_KEY="$(head -c32 /dev/urandom | base64)" \
  ./castlet serve --addr :8080 --base-url http://localhost:8080
```

Open http://localhost:8080, log in at `/login`, create a channel, and upload an
episode. The public pages live at `/{channel}/`, `/{channel}/{episode}/`, and
the feed at `/{channel}/feed.xml`.

## Configuration

Flags (see `castlet serve -h`) or `CASTLET_*` environment variables:

| Flag | Env | Default | Purpose |
|------|-----|---------|---------|
| `--addr` | `CASTLET_ADDR` | `:8080` | listen address |
| `--base-url` | `CASTLET_BASE_URL` | `http://localhost:8080` | absolute root for feed/enclosure URLs |
| `--data-dir` | `CASTLET_DATA_DIR` | `./data` | sqlite file + `media/` blob root |
| `--site-name` | `CASTLET_SITE_NAME` | `Castlet` | name shown in the UI |
| `--transcriber` | `CASTLET_TRANSCRIBER` | `null` | `null` or `command` |
| `--transcribe-command` | `CASTLET_TRANSCRIBE_COMMAND` | — | binary for the command transcriber |
| `--transcribe-args` | `CASTLET_TRANSCRIBE_ARGS` | `{{audio}}` | argument template; `{{audio}}` is the file path |
| `--log-level` | `CASTLET_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| | `CASTLET_SESSION_KEY` | generated | HMAC key for session cookies (set in production) |

Set `CASTLET_SESSION_KEY` (≥32 bytes) in production; otherwise an ephemeral key
is generated and sessions do not survive a restart.

### Transcription

The default `null` transcriber leaves new episodes with a `pending` transcript
status that settles to `none`. To produce real transcripts, point the `command`
transcriber at a tool that prints JSON to stdout:

```json
{ "language": "en", "segments": [ {"start": 0.0, "end": 3.2, "text": "..."} ] }
```

```sh
./castlet serve --transcriber command \
  --transcribe-command my-whisper-wrapper --transcribe-args '--json {{audio}}'
```

## Architecture

See [docs/architecture.md](docs/architecture.md). The swappable seams are
`store.Store` (default SQLite), `blob.BlobStore` (default local FS),
`queue.JobQueue` (default Store-backed), `transcribe.Transcriber` (default
null/command), and `server.Renderer` (default embedded templates). Long-lived
components (`server`, `worker`) use a `Run(ctx) (*Controller, error)` lifecycle;
cancelling the context is the only way to stop them.

## Development

```sh
go test ./...
go vet ./...
```

## License

[PolyForm Noncommercial License 1.0.0](LICENSE).
