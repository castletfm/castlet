# AGENTS.md

Guidance for AI agents (and humans) working in this repo. Read this before making
changes.

Castlet is a **minimal, self-hostable podcast server** in Go, aimed at closed
environments (intranet/team/homelab), not the public internet. It is **work in
progress**; breaking changes (including schema changes that require recreating
the data dir) are acceptable.

## Build, test, run

```sh
go build ./...        # build everything
go test ./...         # all tests (uses testify; sqlite tests use in-memory/temp dirs)
go vet ./...          # vet
go run ./cmd/castlet serve -h     # see all flags

# minimal local run
go run ./cmd/castlet user-create --email you@x.com --password 'pw' --name You
CASTLET_SESSION_KEY="$(head -c32 /dev/urandom | base64)" \
  go run ./cmd/castlet serve --addr :8080 --base-url http://localhost:8080
```

Always run `go build ./...` and `go test ./...` after changes. The CLI subcommands
are `serve`, `migrate`, `user-create`, `version` (`cmd/castlet/main.go`).

For a full demo (OIDC SSO + MinIO media + whisper.cpp transcription) use
`examples/standalone/run.sh` — it spins up containers (needs docker/podman,
ffmpeg, jq) and is the best way to exercise the whole system end to end.

## Layout

- `cmd/castlet` — CLI entry point.
- `app` — **composition root**: builds the swappable backends from `config.Config`
  and wires the server + worker. Backend selection (`buildBlobStore`,
  `buildTranscriber`, `buildAuthenticator`) lives here.
- `config` — flags + `CASTLET_*` env parsing.
- `model` — domain types (User, Channel, Episode, Transcript, Job).
- `store` (`store/sqlite`) — metadata persistence behind `store.Store`.
- `blob` (`blob/localfs`, `blob/s3`) — media object storage behind `blob.BlobStore`.
- `queue` (`queue/dbqueue`) — background job queue behind `queue.JobQueue`.
- `transcribe` (`transcribe/null`, `transcribe/command`) — speech-to-text behind
  `transcribe.Transcriber`.
- `auth` (`auth/oidc`) — OIDC SSO behind `auth.Authenticator`.
- `feed` — RSS/iTunes feed generation.
- `server` — HTTP handlers, routing, `html/template` rendering (`server.Renderer`).
- `web` — embedded templates (`web/templates`) and static assets (`web/static`).
- `worker` — background transcription worker.
- `internal` — small helpers (`idgen`, `session`).
- `docs/architecture.md` — deeper design notes.

## Architecture & conventions

- **Everything external is behind an interface**, swapped only in `app`:
  `store.Store`, `blob.BlobStore`, `queue.JobQueue`, `transcribe.Transcriber`,
  `auth.Authenticator`, `server.Renderer`. Default backends: sqlite + localfs +
  dbqueue + null/command transcriber + embedded templates. Keep new external
  dependencies behind these seams; **prefer the standard library** — e.g.
  `blob/s3` speaks S3 with hand-rolled SigV4, no SDK.
- **Long-lived components** (`server`, `worker`) use a `Run(ctx) (*Controller, error)`
  lifecycle; cancelling the context is the only way to stop them.
- **Server-rendered, no JavaScript build step.** Pages are `html/template`
  (`web/templates`, parsed in `server/render.go`); progressive bits use small
  inline scripts only. Routing is the Go 1.22 `http.ServeMux` pattern syntax.
- **URLs are opaque ids, no slugs.** Channels `/c/{id}/` (+ `/c/{id}/feed.xml`),
  episodes `/e/{id}/`. The id is `idgen.New()`. Don't reintroduce slugs.
- **Media is content-addressed and immutable.** `Episode.MediaKey` is the
  sha256 of the bytes; uploads never replace media. The s3 backend reuses this
  hash to sign uploads. A blob is deleted only when no episode references it
  (`deleteOrphanBlob`).
- **Direct media serving is a startup capability, not a per-request check.** A
  blob store that implements the optional `blob.DirectURL` interface is detected
  once in `server.New` (`s.directBlobs`); when present, `/media/{key}` 302-redirects
  to a presigned URL, otherwise the bytes are streamed. localfs does not implement
  `DirectURL`.
- **Config of backends prefers files, not flag sprawl.** The media blob store is
  picked by one `--blob-store-config <json>` flag whose `type` field selects the
  backend (`fs`|`s3`); don't add `s3-*`/`gcs-*` flags.
- **Transcription** is asynchronous: upload enqueues a `JobTranscribe`; the worker
  runs the configured transcriber (the example shells out to whisper.cpp per
  episode — slow, expect delays). The episode's spoken language is set per episode
  on the upload/edit form and passed through to the transcriber.

## sqlite migrations

`store/sqlite/schema.sql` is applied on every startup (`Migrate`, idempotent
`CREATE TABLE IF NOT EXISTS`). Columns added later are also added via `ALTER TABLE`
in `Migrate` (duplicate-column errors are ignored). When adding a column: update
`schema.sql`, add an `ALTER` to `Migrate`, and update the column list / INSERT /
UPDATE / scan in `store/sqlite/sqlite_crud.go` together.

## Editing templates

`web/templates/*.html` are embedded; each page is parsed against `base.html` in
`server/render.go` and must be registered in `pageNames`. Template funcs
(`languageOptions`, `add`, formatters, …) are registered in the same file.
