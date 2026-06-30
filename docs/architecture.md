# Castlet — Architecture

Castlet is a self-hostable podcast server. It is **minimalist and standalone by
default** (single Go binary, embedded SQLite, local-filesystem media storage,
in-process worker) but every external-facing concern is expressed as an
**interface**, so a deployment can swap in "enterprise" backends (Postgres, S3,
a cloud transcription API, an external job queue) without touching the
application or HTTP layers.

There are no community features (no comments, likes, follows).

## Confirmed decisions

These three forks were decided up front; the rest of the document assumes them.

1. **Frontend: server-rendered Go templates.** `html/template` embedded via
   `embed.FS`, plus ~40 lines of vanilla JS for transcript↔audio sync. No Node
   build step. Rendering sits behind a `server.Renderer` interface so an
   htmx/SPA frontend is an additive swap, not a rewrite.
2. **Transcription default: null, with an opt-in external command.**
   `transcribe/null` performs no work (transcript settles to `none`);
   `transcribe/command` shells out to a configurable whisper.cpp-style binary.
   A fresh install always builds and runs with no heavy dependencies.
3. **Scope: a full working vertical slice.** Upload → blob store → queue →
   worker → public pages + RSS, with auth, admin CRUD, and a CLI.

## Goals & non-goals

- **Single binary, zero required services** for the default install. `go build`
  then `castlet serve` yields a working podcast site.
- **Swappable components** behind narrow interfaces. The default implementation
  of each is the minimalist one; alternatives live in sibling sub-packages.
- **Server-rendered web frontend.** `html/template` + a small amount of vanilla
  JS (transcript ↔ audio sync). No Node/npm build step. Public pages are
  crawlable and shareable.
- **Asynchronous transcription** via a worker that pulls jobs from a queue.
- Non-goals: comments/social, multi-region, horizontal scale-out (the default
  build targets a single node; enterprise backends enable scaling later).

## Domain model

Three primary entities (per the design overview) plus transcripts and jobs.

```
User 1───* Channel 1───* Episode 1───0..1 Transcript 1───* Segment
```

- **User** — a person who creates podcasts. Owns channels. Authenticates to the
  admin area. `id, email, display_name, password_hash, created_at`.
- **Channel** — groups episodes; a user may own many. Has a public `slug`.
  `id, user_id, slug, title, description, language, image_path, created_at,
  updated_at`.
- **Episode** — a single media item, **audio or video**. Belongs to one
  channel; has a public `slug` unique within the channel. `id, channel_id, slug,
  title, description, media_key, media_mime, media_kind (audio|video),
  media_bytes, duration_secs, status (draft|published), transcript_status
  (none|pending|processing|done|failed), published_at, created_at, updated_at`.
  `media_kind` is derived from the upload's content type and selects the
  player: the episode page renders `<video>` for video and `<audio>` for audio.
- **Transcript** — produced asynchronously for an episode. Stored as ordered
  **Segments** `{start_secs, end_secs, text}`, which drive the click-to-seek UI.
- **Job** — a unit of async work (currently only `transcribe`) with a status
  lifecycle, attempt count, and an opaque JSON payload.

Identifiers are 16-byte random values rendered as Crockford base32 (sortable
enough, URL-safe, collision-resistant). Slugs are human-facing and separate.

## Package layout

```
cmd/castlet/            CLI entry point: serve | migrate | user-create | version
app/                    composition root: builds backends from Config, wires
                        Server + Worker, owns their lifecycles
config/                 Config struct; load from env + flags

model/                  domain types + enums (no I/O)

store/                  Store interface (metadata persistence) + errors/filters
  store/sqlite/         default: modernc.org/sqlite (pure Go, cgo-free)
blob/                   BlobStore interface (opaque binary objects: audio, images)
  blob/localfs/         default: local filesystem
queue/                  JobQueue interface (enqueue/claim/ack/retry)
  queue/dbqueue/        default: backed by Store (no extra service)
transcribe/             Transcriber interface + Result/Segment types
  transcribe/null/      default: no-op (leaves transcript "pending")
  transcribe/command/   opt-in: shells out to a whisper.cpp-style binary

worker/                 transcription worker (Run/Controller lifecycle)
feed/                   RSS 2.0 + iTunes podcast feed generation
server/                 HTTP server (Run/Controller lifecycle), router,
                        handlers, Renderer interface, sessions, middleware
internal/idgen/         random id generation
internal/session/       HMAC-signed cookie sessions (swap target: server-side)

web/templates/          embedded html/template files
web/static/             embedded css + js
```

### Why these seams (and not others)

The interfaces are drawn where the *operational dependency* changes, not where
the code is merely large:

| Interface     | Default (minimalist) | Enterprise swap | Boundary rationale |
|---------------|----------------------|-----------------|--------------------|
| `store.Store` | SQLite file          | Postgres        | metadata DB is the classic scale seam |
| `blob.BlobStore` | local FS          | S3/GCS          | media is large, often offloaded to object storage/CDN |
| `queue.JobQueue` | DB-polling queue  | Redis/SQS       | async fan-out is where you outgrow a single node first |
| `transcribe.Transcriber` | null/command | cloud API  | transcription is the heaviest compute; many hosting options |
| `server.Renderer` | html/template    | (htmx / SPA API) | keeps a future frontend swap additive |

Auth/session is also behind `internal/session` so cookie sessions can become
server-side/SSO later.

## Component contracts (lifecycle)

Long-lived components follow the house **Run/Controller** pattern
(`~/.claude/docs/go-server-lifecycle.md`): `Run(ctx) (*Controller, error)` binds
synchronously and spawns the work goroutine; cancelling `ctx` is the only stop
signal; the `Controller` exposes `Done() / Err() / Wait()`. This applies to both
`server.Server` and `worker.Worker`. Receivers hold only validated config and
are safe to `Run` repeatedly.

Constructors take options via `github.com/lestrrat-go/option/v3` (ident structs
+ `option.MustGet`), folded into the receiver in the constructor.

The `queue.JobQueue` contract keeps callers ignorant of retry policy: callers
only `Ack` or `Nack(jobID, cause)`, and the queue decides whether to reschedule
with backoff or mark the job permanently dead (`Nack` returns a `dead bool`).
The default `dbqueue` implements this over the metadata `Store`, which is why
`Store` carries the job methods (`EnqueueJob`, `JobByID`, `ClaimJob`,
`CompleteJob`, `RescheduleJob`, `FailJob`). `ClaimJob` leases a job (marks it
processing and pushes `run_after` out) so a crashed worker's job becomes
reclaimable. `Store` also exposes `EpisodeByAudioKey` so the media endpoint can
serve a blob with the correct content type without threading metadata through
the blob layer.

## Request & job flows

**Publish an episode (admin):**
1. `POST /admin/channels/{ch}/episodes` (multipart) → handler validates, streams
   the upload into `BlobStore.Put` → gets a `media_key`; `media_kind` is derived
   from the upload's content type (audio or video).
2. `Store.CreateEpisode` persists metadata (`status=draft`,
   `transcript_status=pending`).
3. Handler enqueues a `transcribe` job via `JobQueue.Enqueue`.
4. Admin publishes → `status=published`, `published_at=now`.

**Transcription (worker):**
1. `Worker` loop calls `JobQueue.Claim` (atomic; marks job `processing` with a
   lease) → gets a `transcribe` job.
2. Loads episode + opens audio via `BlobStore.Get`.
3. Calls `Transcriber.Transcribe(ctx, audio) → Result{Segments}`.
   - null transcriber returns `ErrUnsupported` → job is acked as "skipped",
     episode `transcript_status=none`.
4. `Store.SaveTranscript` writes segments; episode `transcript_status=done`.
5. `JobQueue.Ack` (or `Retry` with backoff on transient error; `Fail` after max
   attempts → `transcript_status=failed`).

**View an episode (public):**
- `GET /{channel-slug}/{episode-slug}` renders title/description, a player
  (`<video>` for video episodes, `<audio>` for audio) pointing at
  `GET /media/{media_key}` (served via `BlobStore`, range-aware), and the
  transcript. `player.js` highlights the active segment on `timeupdate` and
  seeks on segment click; it binds to the `#player` element regardless of media
  kind.

**Feeds:** `GET /{channel-slug}/feed.xml` builds an RSS/iTunes document from the
channel + its published episodes (`feed` package), enclosures point at the media
URL.

## HTTP surface

```
GET  /                                          landing: list channels
GET  /{channel}/                                channel page: published episodes
GET  /{channel}/feed.xml                        RSS feed
GET  /{channel}/{episode}/                       episode page (player + transcript)
GET  /media/{key}                               audio/image bytes (range requests)

GET  /login   POST /login                       session login
POST /logout

GET  /admin/                                     dashboard: your channels (auth)
GET  /admin/channels/new                         new-channel form
POST /admin/channels                             create channel
GET  /admin/channels/{id}/edit                   edit-channel form
POST /admin/channels/{id}                         update channel
GET  /admin/channels/{id}/episodes               list a channel's episodes
GET  /admin/channels/{id}/episodes/new           new-episode (upload) form
POST /admin/channels/{id}/episodes               create episode (multipart)
POST /admin/episodes/{id}/publish                 publish
POST /admin/episodes/{id}/unpublish               unpublish
POST /admin/episodes/{id}/delete                  delete (also deletes the blob)
```

Routing uses the Go 1.22+ `net/http.ServeMux` method+pattern matcher (no router
dependency); a literal first segment (`admin`, `login`, `media`, `static`)
beats the `{channel}` wildcard, and those names are reserved so a channel slug
can never shadow them. `loadUser` middleware resolves the session cookie into a
context user for every request; `requireAuth` gates the `/admin/...` handlers.
Admin handlers re-check that the target channel/episode is owned by the current
user. CSRF is mitigated by `SameSite=Lax` session cookies plus POST-only
mutations; a token scheme is a noted follow-up.

## Configuration

`config.Config` is populated from flags then environment (`CASTLET_*`). Key
fields: `Addr`, `DataDir` (sqlite file + blob root live under it by default),
`BaseURL` (for absolute feed/enclosure URLs), `SessionKey`, `StoreDSN`,
`BlobBackend`, `Transcriber` (+ `TranscribeCommand`). Unset secrets in dev are
generated and a warning logged; production must set `CASTLET_SESSION_KEY`.

## Testing strategy

- Store/blob/queue each ship a focused unit test against their default impl
  (SQLite in a temp file, blob in a temp dir).
- `Transcriber` null impl test asserts `ErrUnsupported`.
- `feed` has a golden-ish structural test.
- External-package tests (`xxx_test`), `testify/require`, `t.Context()`.

## Licensing

Polyform Noncommercial License 1.0.0 (`LICENSE`). Source headers are not
required by the license; the repository-level `LICENSE` governs.
