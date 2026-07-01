-- Castlet schema v1. All statements are idempotent so Migrate can run on every
-- startup. Timestamps are stored as Unix seconds (INTEGER).

CREATE TABLE IF NOT EXISTS users (
    id            TEXT    PRIMARY KEY,
    email         TEXT    NOT NULL UNIQUE,
    display_name  TEXT    NOT NULL,
    password_hash TEXT    NOT NULL,
    oidc_issuer   TEXT    NOT NULL DEFAULT '',
    oidc_subject  TEXT    NOT NULL DEFAULT '',
    session_epoch INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);
-- The oidc/session_epoch columns and this index are also ensured imperatively
-- in Migrate so databases created before they existed are upgraded in place.

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
