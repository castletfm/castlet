# Castlet — Operations

Operator guide for running Castlet beyond the README quickstart. Castlet is a
single binary that runs the web server and the transcription worker in one
process (`castlet serve`). It is designed for a **single node** in a closed
environment, not a horizontally scaled public deployment.

All flags below have `CASTLET_*` environment fallbacks; flags win over env.
Run `castlet serve -h` for the full list. See `config/config.go` for the source
of truth.

## Configuration & secrets

### Session key (required in production)

`CASTLET_SESSION_KEY` signs the HMAC session cookies. It is an **environment
variable only** — there is no flag for it.

- It **must be set and stable across restarts.** If unset, Castlet generates a
  random ephemeral key at startup and logs a warning; every existing session is
  invalidated on the next restart, forcing all users to log in again.
- Use **at least 32 bytes** of high-entropy secret (that is the length of the
  auto-generated key and what the session code expects). For example:

  ```sh
  CASTLET_SESSION_KEY="$(head -c32 /dev/urandom | base64)"
  ```

Keep the value out of your shell history and process listings where possible
(e.g. a systemd `EnvironmentFile` or a container secret).

### Base URL and the Secure cookie flag

`--base-url` (`CASTLET_BASE_URL`, default `http://localhost:8080`) is the
absolute site root used for feed/enclosure URLs **and** it controls the session
cookie's `Secure` flag: the cookie is marked `Secure` (sent over HTTPS only)
**iff** the base URL scheme is `https://`. Set an `https://` base URL whenever
users reach Castlet over TLS (see Reverse proxy below), otherwise browsers may
drop the cookie or send it over cleartext.

### Local sign-up

`--allow-signup` (`CASTLET_ALLOW_SIGNUP`, default **true**) enables the
self-service local sign-up path. Set it to `false` on shared/closed
deployments where accounts should be provisioned only via `castlet user-create`
or OIDC.

### OIDC single sign-on

OIDC is enabled when an issuer is configured; otherwise it is off. When enabled,
the client id and client secret are also **required** — startup fails without
them. Discovery runs once at startup with a 15s timeout, so an unreachable issuer
fails the boot.

| Flag | Env | Notes |
|------|-----|-------|
| `--oidc-issuer` | `CASTLET_OIDC_ISSUER` | Issuer URL; **enables SSO when set** |
| `--oidc-client-id` | `CASTLET_OIDC_CLIENT_ID` | **Required** when OIDC is enabled |
| `--oidc-client-secret` | `CASTLET_OIDC_CLIENT_SECRET` | **Required** when OIDC is enabled |
| `--oidc-redirect-url` | `CASTLET_OIDC_REDIRECT_URL` | Defaults to `<base-url>/auth/oidc/callback` |
| `--oidc-scopes` | `CASTLET_OIDC_SCOPES` | Space-separated; defaults to `openid profile email` |

### Media blob store (`fs` | `s3`)

The media object store is selected by a **single** flag,
`--blob-store-config` (`CASTLET_BLOB_STORE_CONFIG`), whose value is the path to
a **JSON file**. The file's `type` field picks the backend:

- Unset / empty → local filesystem under `<data-dir>/media`.
- `{"type":"fs","dir":"..."}` → local filesystem (`dir` defaults to
  `<data-dir>/media`).
- `{"type":"s3", ...}` → S3-compatible object storage. Fields:
  `endpoint`, `region` (defaults `us-east-1`), `bucket`, `access_key`,
  `secret_key`, `prefix` (optional key prefix). `endpoint`, `bucket`,
  `access_key`, and `secret_key` are required.

Do not look for `s3-*`/`gcs-*` flags — there are none; everything goes in this
one JSON file.

### Other useful flags

`--addr` (listen address), `--data-dir` (holds the sqlite file and, by default,
the media root), `--site-name`, `--log-level` (`debug|info|warn|error`), and the
transcription flags `--transcriber` (`null`|`command`), `--transcribe-command`,
`--transcribe-args`.

## Reverse proxy / TLS

Castlet serves plain HTTP and has no built-in TLS. In production, run it behind
a TLS-terminating reverse proxy (nginx, Caddy, Traefik, a cloud LB, …) and:

- Terminate TLS at the proxy and forward to Castlet's `--addr`.
- Set `--base-url` to the **public `https://` URL**. This is what makes the
  session cookie `Secure` (see above) and produces correct absolute feed and
  enclosure URLs.
- If OIDC is enabled, ensure the redirect URL registered with your provider
  matches `<base-url>/auth/oidc/callback` (or your explicit
  `--oidc-redirect-url`).

## Backup & restore

Castlet's state is **two things that must be backed up together and
consistently**:

1. The **SQLite database** — `castlet.db` in the data dir (plus its
   `castlet.db-wal` and `castlet.db-shm` sidecars, since the DB runs in **WAL
   mode**).
2. The **media blobs** — content-addressed, immutable files under
   `<data-dir>/media` for the `fs` backend, or in your S3 bucket/prefix for the
   `s3` backend.

The database references blobs by content hash (`Episode.MediaKey`), so a backup
that captures one but not the other will have dangling or missing media. For a
**consistent snapshot**, either:

- **Stop the service** (`castlet serve` shuts down cleanly on SIGINT/SIGTERM),
  then copy the data dir (and/or snapshot the S3 bucket); or
- Use SQLite's **online backup** / `VACUUM INTO` to get a consistent DB copy
  while running — do **not** just `cp` the `.db` file mid-write, as WAL sidecars
  may be inconsistent — and capture the media at the same point in time.

When media lives on S3, back up (or version) that bucket/prefix **alongside**
the database snapshot, not independently.

Restore is the reverse: put the data dir (and/or S3 contents) back in place and
start Castlet; it will re-run migrations idempotently.

## Upgrades

Schema migrations run **automatically and idempotently on every startup** (both
`serve` and `migrate` call `Migrate`). Migration uses
`CREATE TABLE IF NOT EXISTS` plus `ALTER TABLE ... ADD COLUMN` for later
columns (duplicate-column errors are ignored), so restarting a newer binary on
an existing data dir upgrades it in place.

**WIP caveat:** Castlet is work in progress. Breaking schema changes that
require **recreating the data dir** are currently considered acceptable between
versions. Take a backup before upgrading, read the release/commit notes, and be
prepared to start from a fresh data dir if a change is not backward compatible.

## Scaling ceiling (single node)

Castlet is intentionally single-node and is **not horizontally scalable as-is**:

- The SQLite store is opened with **a single database connection**
  (`SetMaxOpenConns(1)`) in WAL mode with a busy timeout. That one connection
  serializes **all** DB access — reads and writes alike — which avoids
  "database is locked" errors but caps throughput.
- The transcription worker processes jobs **serially** (one at a time, polling
  the DB-backed queue). Transcription (e.g. whisper.cpp) is slow, so a backlog
  of uploads is worked through sequentially.

Running multiple `castlet serve` processes against the same data dir is **not**
supported. If you outgrow one node, that is the point to swap the store/blob
backends behind their interfaces (see `docs/architecture.md`), not to run
replicas.

## Process supervision

The two goroutine classes fail differently:

- **HTTP handlers**: two layers keep a handler panic from taking down the
  process. Go's `net/http` already recovers a panic in the per-connection
  handler goroutine (logging it and closing that one connection). On top of
  that, Castlet installs an app-level `recover()` middleware (`recoverPanic` in
  `server/middleware.go`): it catches a panic in any handler, logs it with a
  stack trace, and returns a 500 (re-panicking `http.ErrAbortHandler`, and
  aborting the connection if a partial response was already written). Either
  way, one bad request keeps serving other requests.
- **Background transcription worker**: job dispatch is **not** wrapped in a
  `recover()`, so a panic in a job (or a fatal error) can terminate the process.

Run Castlet under a restart supervisor primarily so that worker panics and fatal
errors don't leave it down:

- **systemd**: a unit with `Restart=on-failure` (or `always`) and the secrets
  in an `EnvironmentFile`.
- **Containers**: a restart policy such as `--restart=unless-stopped` (Docker/
  Podman) or a Kubernetes Deployment/`restartPolicy: Always`.

Castlet stops cleanly on SIGINT/SIGTERM (graceful HTTP shutdown and worker
wind-down), so supervised stop/restart is safe.
