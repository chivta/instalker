CREATE TABLE IF NOT EXISTS seen_media (
    owner_pk   TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    media_id   TEXT    NOT NULL,
    taken_at   INTEGER NOT NULL,
    seen_at    INTEGER NOT NULL,
    PRIMARY KEY (owner_pk, kind, media_id)
);

CREATE INDEX IF NOT EXISTS seen_media_seen_at_idx ON seen_media (seen_at);

CREATE TABLE IF NOT EXISTS watch_state (
    owner_pk      TEXT PRIMARY KEY,
    username      TEXT NOT NULL,
    initialized   INTEGER NOT NULL DEFAULT 0
);
