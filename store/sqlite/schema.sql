-- Castlet schema v1. All statements are idempotent so Migrate can run on every
-- startup. Timestamps are stored as Unix seconds (INTEGER).

CREATE TABLE IF NOT EXISTS users (
    id            TEXT    PRIMARY KEY,
    email         TEXT    NOT NULL,
    display_name  TEXT    NOT NULL,
    password_hash TEXT    NOT NULL,
    oidc_issuer   TEXT    NOT NULL DEFAULT '',
    oidc_subject  TEXT    NOT NULL DEFAULT '',
    session_epoch INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);
-- Email uniqueness is case-INSENSITIVE (COLLATE NOCASE): the identity model is
-- one mailbox = one account, so Alice@x and alice@x must collide. Writers also
-- store the canonicalized (lower-cased) address (see internal/email), so this
-- index is defense in depth for any path that reaches the store directly. This
-- index and the oidc/session_epoch columns are also ensured imperatively in
-- Migrate so databases created before they existed are upgraded in place.
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users(email COLLATE NOCASE);

CREATE TABLE IF NOT EXISTS channels (
    id          TEXT    PRIMARY KEY,
    user_id     TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    language    TEXT    NOT NULL DEFAULT 'en',
    image_key   TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_channels_user ON channels(user_id);

CREATE TABLE IF NOT EXISTS episodes (
    id                TEXT    PRIMARY KEY,
    channel_id        TEXT    NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    title             TEXT    NOT NULL,
    description       TEXT    NOT NULL DEFAULT '',
    media_key         TEXT    NOT NULL DEFAULT '',
    media_mime        TEXT    NOT NULL DEFAULT '',
    media_kind        TEXT    NOT NULL DEFAULT 'audio',
    media_bytes       INTEGER NOT NULL DEFAULT 0,
    duration_secs     INTEGER NOT NULL DEFAULT 0,
    language          TEXT    NOT NULL DEFAULT '',
    position          INTEGER NOT NULL DEFAULT 0,
    status            TEXT    NOT NULL,
    transcript_status TEXT    NOT NULL,
    published_at      INTEGER,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_episodes_channel ON episodes(channel_id);

CREATE TABLE IF NOT EXISTS transcripts (
    episode_id TEXT    PRIMARY KEY REFERENCES episodes(id) ON DELETE CASCADE,
    language   TEXT    NOT NULL DEFAULT '',
    segments   TEXT    NOT NULL DEFAULT '[]', -- JSON array of {start,end,text}
    created_at INTEGER NOT NULL
);

-- blob_reservations records an in-flight media upload. A row is written before
-- the blob is stored (blobs.Put) and removed once the episode row that will
-- reference it is committed. Because the blob is written before its episode
-- exists, an episode delete's orphan check would otherwise not see a concurrent
-- upload; counting active reservations closes that window. Identical
-- (content-addressed) concurrent uploads each add a row, so the table acts as a
-- refcount per key. The autoincrement id is the release token: ReleaseBlob drops
-- exactly the caller's row, never an arbitrary one. A row left behind by a crashed
-- upload is ignored once older than the reservation TTL (see blobReservationTTL in
-- sqlite_crud.go), so a leak only delays orphan cleanup rather than pinning a blob
-- forever. This table is created by Migrate (which executes this schema) on new and
-- existing databases.
CREATE TABLE IF NOT EXISTS blob_reservations (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    media_key  TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_blob_reservations_key ON blob_reservations(media_key);

-- blob_delete_leases records an in-progress PHYSICAL blob delete (a tombstone).
-- The orphan decision and the lease acquisition happen in one transaction; the
-- lease is held across the out-of-transaction blobs.Delete and released only
-- after. A concurrent ReserveBlob for the same key serializes against the lease
-- and is rejected (store.ErrBlobDeleting) until the delete finishes, closing the
-- window where a reservation created between the delete tx commit and the physical
-- delete would be invisible. Acquisition is single-winner (only the first deleter
-- takes the lease while it is active), and the autoincrement id is the owner
-- token: ReleaseDeleteLease drops exactly that row, so one deleter can never clear
-- another deleter's still-active lease. A lease from a crashed delete handler is
-- ignored once older than store.BlobDeleteLeaseTTL.
CREATE TABLE IF NOT EXISTS blob_delete_leases (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    media_key  TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_blob_delete_leases_key ON blob_delete_leases(media_key);

CREATE TABLE IF NOT EXISTS jobs (
    id         TEXT    PRIMARY KEY,
    kind       TEXT    NOT NULL,
    payload    TEXT    NOT NULL DEFAULT '',
    status     TEXT    NOT NULL,
    attempts   INTEGER NOT NULL DEFAULT 0,
    last_error TEXT    NOT NULL DEFAULT '',
    run_after  INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_claim ON jobs(status, run_after);
